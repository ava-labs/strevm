package sae

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sync"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	safemath "github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"
)

type (
	sig struct{}
	ack struct{}
)

type executor struct {
	chain *Chain

	accepted <-chan blockAcceptance
	chunks   chan<- *chunk
	quit     <-chan sig
	done     chan<- ack

	spawned sync.WaitGroup

	queue        sink.Mutex[[]*Block]
	queuePending chan sig

	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
}

func (e *executor) init() error {
	db := state.NewDatabase(rawdb.NewMemoryDatabase())
	statedb, err := state.New(types.EmptyRootHash, db, nil)
	if err != nil {
		return err
	}

	e.executeScratchSpace = executionScratchSpace{
		statedb: statedb,
		chunk:   new(chunk),
		gasConfig: gas.Config{
			MaxPerSecond:             1e9,
			ExcessConversionConstant: 0, // TODO(arr4n)
		},
	}
	return nil
}

func (e *executor) start() {
	e.queue = sink.NewMutex[[]*Block](nil)
	e.queuePending = make(chan sig, 1)
	e.spawn(e.enqueueAccepted)
	e.spawn(e.clearQueue)

	<-e.quit
	e.spawned.Wait()
	close(e.chunks)
	close(e.queuePending)
	e.queue.Close()

	close(e.done)
}

func (e *executor) logger() logging.Logger {
	return e.chain.snowCtx.Log
}

func (e *executor) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
}

type blockAcceptance struct {
	// This Context MUST be ephemeral and immediately dropped at the end of the
	// `case` in which it is received. Snowman will call [Block.Accept] with a
	// Context, at which point the block will be sent over a channel, the
	// receiver of which requires a Context (this one). Without refactoring the
	// pattern to being a function call, there is no other way to propagate the
	// Context.
	ctx   context.Context
	block *Block
	errCh chan error
}

func (e *executor) enqueueAccepted() {
	for {
		a, ok := recv(e.quit, e.accepted)
		if !ok {
			return
		}
		a.errCh <- e.queue.Replace(a.ctx, func(q []*Block) ([]*Block, error) {
			q = append(q, a.block)
			e.spawn(e.signalQueuePush)
			return q, nil
		})
	}
}

func (e *executor) signalQueuePush() {
	send(e.quit, e.queuePending, sig{})
}

func (e *executor) clearQueue() {
	ctx, cancel := context.WithCancel(context.Background())
	e.spawn(func() {
		<-e.quit
		cancel()
	})

	for {
		if _, ok := recv(e.quit, e.queuePending); !ok {
			return
		}

		var b *Block
		err := e.queue.Replace(ctx, func(q []*Block) ([]*Block, error) {
			b = q[0]
			return q[1:], ctx.Err()
		})
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			// [sink.Mutex.Replace] will only return the [context.Context] error
			// or the error returned by its argument, so this is theoretically
			// impossible but included for completeness to be detected in tests.
			e.logger().Fatal("BUG: popping from queue", zap.Error(err))
		}

		if err := e.execute(b); err != nil {
			e.logger().Fatal(
				"Executing enqueued block",
				zap.Error(err),
				zap.Uint64("height", b.Height()),
				zap.Uint64("timestamp", b.b.Time()),
				zap.Any("hash", b.b.Hash()),
			)
			return
		}
	}
}

type executionScratchSpace struct {
	statedb   *state.StateDB
	chunk     *chunk
	gasPool   core.GasPool
	gasConfig gas.Config
	excess    gas.Gas
}

func (x *executionScratchSpace) chunkCapacity() gas.Gas {
	return x.chunk.donation + x.gasConfig.MaxPerSecond
}

func (x *executionScratchSpace) gasPrice() gas.Price {
	return gas.CalculatePrice(
		x.gasConfig.MinPrice,
		x.excess,
		x.gasConfig.ExcessConversionConstant,
	)
}

type chunk struct {
	stateRoot          common.Hash
	receipts           []*types.Receipt
	consumed, donation gas.Gas
	time               uint64 // same type as [types.Header.Time]
}

func (e *executor) execute(b *Block) error {
	x := &e.executeScratchSpace
	x.gasPool.SetGas(math.MaxUint64) // necessary but unused so max it out

	header := types.CopyHeader(b.b.Header())
	header.BaseFee = new(big.Int)

	if header.Time > x.chunk.time {
		e.logger().Fatal(
			"BUG: chunk executing the future",
			zap.Uint64("block time", header.Time),
			zap.Uint64("chunk time", x.chunk.time),
		)
	}

	for ti, tx := range b.b.Transactions() {
		header.BaseFee.SetUint64(uint64(x.gasPrice()))
		x.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			params.TestChainConfig,
			e.chain,
			&header.Coinbase,
			&x.gasPool,
			x.statedb,
			header,
			tx,
			(*uint64)(&x.chunk.consumed),
			vm.Config{},
		)
		if err != nil {
			return err
		}

		x.excess += gas.Gas(receipt.GasUsed >> 1) // see ACP for details

		if x.chunk.consumed < x.chunkCapacity() {
			x.chunk.receipts = append(x.chunk.receipts, receipt)
			continue
		}
		// The overflowing tx is going to be in the next chunk so reduce the
		// current one's consumed gas.
		x.chunk.consumed -= gas.Gas(receipt.GasUsed)

		if !e.nextChunk(x, receipt) {
			return context.Canceled
		}
	}

	// TODO(arr4n): this needs additional checks, to see if the queue was
	// exhausted.
	if !e.nextChunk(x, nil) {
		return context.Canceled
	}
	return nil
}

func (e *executor) nextChunk(x *executionScratchSpace, overflowTx *types.Receipt) bool {
	// Although gas surplus won't be used until later, it MUST be calculated
	// before we overwrite the chunk.
	surplus, err := safemath.Sub(x.chunkCapacity(), x.chunk.consumed)
	if err != nil {
		e.logger().Fatal(
			"BUG: Over-filled chunk",
			zap.Any("capacity", x.chunkCapacity()),
			zap.Any("consumed", x.chunk.consumed),
		)
	}

	x.chunk.stateRoot = x.statedb.IntermediateRoot(true)
	if !send(e.quit, e.chunks, x.chunk) {
		return false
	}
	x.chunk = new(chunk)

	if overflowTx == nil {
		// The queue was exhausted so the gas excess is reduced, but we MUST NOT
		// donate the surplus to the next chunk because the execution stream is
		// now dormant.
		xs := x.excess - (surplus >> 1)
		if xs > x.excess { // underflow
			xs = 0
		}
		x.excess = xs

	} else {
		// Conversely, the surplus is being used for the overflowing transaction
		// so MUST be donated to the next chunk and MUST NOT count as a
		// reduction in gas excess.
		x.chunk.donation = surplus
		if x.chunkCapacity() < gas.Gas(overflowTx.GasUsed) {
			e.logger().Fatal("Unimplemented: empty interim chunk for XL tx")
		}
		x.chunk.receipts = []*types.Receipt{overflowTx}
	}

	return true
}
