package sae

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sync"
	"time"

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

	"github.com/ava-labs/strevm/queue"
)

const maxGasPerChunk = 100_000

type (
	sig struct{}
	ack struct{}
)

type executor struct {
	chain *Chain

	accepted <-chan blockAcceptance
	quit     <-chan sig
	done     chan<- ack

	spawned sync.WaitGroup

	queue        sink.Mutex[*queue.FIFO[*Block]]
	queuePending chan sig
	chunks       sink.Mutex[map[uint64]*chunk]
	chunkFilled  chan sig

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
			MaxPerSecond:             maxGasPerChunk,
			ExcessConversionConstant: 0, // TODO(arr4n)
		},
	}
	return nil
}

func (e *executor) start() {
	e.queue = sink.NewMutex(new(queue.FIFO[*Block]))
	e.queuePending = make(chan sig, 1)
	e.chunks = sink.NewMutex(make(map[uint64]*chunk))
	const d = 1 // ACP variable
	e.chunkFilled = make(chan sig, d)
	e.spawn(e.enqueueAccepted)
	e.spawn(e.clearQueue)

	<-e.quit
	e.spawned.Wait()
	close(e.chunkFilled)
	e.chunks.Close()
	close(e.queuePending)
	e.queue.Close()

	close(e.done)
}

func (e *executor) quitCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	e.spawn(func() {
		<-e.quit
		cancel()
	})
	return ctx
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
		a.errCh <- e.queue.Use(a.ctx, func(q *queue.FIFO[*Block]) error {
			q.Push(a.block)
			e.spawn(e.signalQueuePush)
			return nil
		})
	}
}

func (e *executor) signalQueuePush() {
	send(e.quit, e.queuePending, sig{})
}

func (e *executor) signalChunkFilled() {
	send(e.quit, e.chunkFilled, sig{})
}

func (e *executor) clearQueue() {
	for {
		if _, ok := recv(e.quit, e.queuePending); !ok {
			return
		}

		block, err := sink.FromMutex(e.quitCtx(), e.queue, func(q *queue.FIFO[*Block]) (*Block, error) {
			b, ok := q.Pop()
			if !ok {
				// The recv() above is effectively a CondVar so if this happens
				// then we've wired things up incorrectly.
				return nil, errors.New("BUG: empty block queue")
			}
			return b, nil
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

		if err := e.execute(block); err != nil {
			e.logger().Fatal(
				"Executing accepted block",
				zap.Error(err),
				zap.Uint64("height", block.Height()),
				zap.Uint64("timestamp", block.b.Time()),
				zap.Any("hash", block.b.Hash()),
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
	time               uint64    // same type as [types.Header.Time]
	filledBy           time.Time // wall time for metrics
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

	e.logger().Debug(
		"Executing accepted block",
		zap.Uint64("height", b.Height()),
		zap.Uint64("timestamp", header.Time),
		zap.Int("transactions", len(b.b.Transactions())),
	)

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

	// Let C and B be the timestamps of the chunk and the block (header),
	// respectively:
	//
	// * B < C: execution is slower than blocks so the queue is not exhausted
	// * C == B: queue exhausted per criterion (1) of the ACP
	// * B > C: invalid (enforced earlier)
	if x.chunk.time == header.Time {
		if !e.nextChunk(x, nil) {
			return context.Canceled
		}
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
	x.chunk.filledBy = time.Now()
	e.logger().Debug(
		"Concluding chunk",
		zap.Uint64("timestamp", x.chunk.time),
	)
	err = e.chunks.Use(e.quitCtx(), func(cs map[uint64]*chunk) error {
		cs[x.chunk.time] = x.chunk
		e.spawn(e.signalChunkFilled)
		return nil
	})
	if err != nil {
		return false
	}

	x.chunk = &chunk{
		time: x.chunk.time + 1,
	}
	if tx := overflowTx; tx != nil {
		// The queue wasn't exhausted so surplus gas is being used for the
		// overflowing transaction and MUST be donated to the next chunk but
		// MUST NOT count as a reduction in gas excess.
		x.chunk.donation = surplus
		x.chunk.consumed += gas.Gas(tx.GasUsed)
		if x.chunkCapacity() < gas.Gas(tx.GasUsed) {
			e.logger().Fatal("Unimplemented: empty interim chunk for XL tx")
		}
		x.chunk.receipts = []*types.Receipt{tx}
	} else {
		// Conversely, the queue was exhausted so the gas excess is reduced, but
		// we MUST NOT donate the surplus to the next chunk because the
		// execution stream is now dormant.
		xs := x.excess - (surplus >> 1)
		if xs > x.excess { // underflow
			xs = 0
		}
		x.excess = xs
	}

	return true
}
