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

type executor struct {
	chain   *Chain
	running chan<- struct{} // closed by [executor.start] to signal to [Chain.Initialize]
	quit    <-chan struct{}
	done    chan<- struct{}

	spawned sync.WaitGroup

	queue  sink.Monitor[*queue.FIFO[*Block]]
	chunks sink.Monitor[map[uint64]*chunk]

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
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.chunks = sink.NewMonitor(make(map[uint64]*chunk))
	e.spawn(e.clearQueue)

	close(e.running)

	<-e.quit
	e.spawned.Wait()
	e.chunks.Close()
	e.queue.Close()

	close(e.done)
}

func (e *executor) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
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

// enqueueAccepted is intended to be called by [Block.Accept], passing itself
// as the first argument. The `parent` MAY be nil i.f.f `block` is the genesis.
func (e *executor) enqueueAccepted(ctx context.Context, block, parent *Block) error {
	var blocks []*Block
	if block.b.NumberU64() == 0 {
		// Genesis, by definition, has no previous blocks but we need to
		// trigger the time-zero chunk.
		blocks = []*Block{block}
	} else {
		// Execution chunks are indexed by timestamp, not by block height,
		// so they need to know the state of the queue at each second.
		// Therefore, to signal that there were no transactions between this
		// accepted block and the last one, we nudge the execution stream
		// with empty pseudo-blocks that can trigger its chunk-filling
		// criterion for an exhausted queue.
		lastTime := parent.b.Time()
		gap := block.b.Time() - lastTime

		blocks = make([]*Block, gap)
		blocks[gap-1] = block

		for i := range uint64(gap - 1) {
			blocks[i] = &Block{
				b: types.NewBlockWithHeader(&types.Header{
					Time: lastTime + i + 1,
				}),
			}
		}
	}

	return e.queue.UseThenSignal(ctx, func(q *queue.FIFO[*Block]) error {
		for _, b := range blocks {
			q.Push(b)
		}
		return nil
	})
}

func (e *executor) clearQueue() {
	for {
		var block *Block
		err := e.queue.Wait(e.quitCtx(),
			func(q *queue.FIFO[*Block]) bool {
				return q.Len() > 0
			},
			func(q *queue.FIFO[*Block]) error {
				// We know that q.Len() > 0 so there's no need to check Pop()'s
				// boolean.
				block, _ = q.Pop()
				return nil
			},
		)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			// [sink.Monitor.Wait] will only return the [context.Context] error
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
		zap.Int("receipts", len(x.chunk.receipts)),
		zap.Bool("queue_exhausted", overflowTx == nil),
		zap.Uint64("gas_capacity", uint64(x.chunkCapacity())),
		zap.Uint64("gas_consumed", uint64(x.chunk.consumed)),
	)
	err = e.chunks.UseThenSignal(e.quitCtx(), func(cs map[uint64]*chunk) error {
		cs[x.chunk.time] = x.chunk
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
		x.excess = clippedSubtract(x.excess, surplus>>1)
	}

	return true
}
