package sae

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/queue"
)

type executor struct {
	chain *Chain
	quit  <-chan struct{}
	done  chan<- struct{}

	spawned sync.WaitGroup

	queue  sink.Monitor[*queue.FIFO[*Block]]
	chunks sink.Monitor[map[uint64]*chunk]

	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
	snaps               sink.Monitor[*snapshot.Tree]
}

func (e *executor) init(ctx context.Context, genesis *core.Genesis) error {
	db := rawdb.NewMemoryDatabase()
	sdb := state.NewDatabase(db)
	tdb := sdb.TrieDB()
	chainConfig, genesisHash, err := core.SetupGenesisBlock(db, tdb, genesis)
	if err != nil {
		return err
	}

	genesisBlock := rawdb.ReadBlock(db, genesisHash, 0)
	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: false,
	}
	snaps, err := snapshot.New(snapConf, db, tdb, genesisBlock.Root())
	if err != nil {
		return err
	}
	e.snaps = sink.NewMonitor(snaps)
	statedb, err := state.New(genesisBlock.Root(), sdb, snaps)
	if err != nil {
		return err
	}

	e.executeScratchSpace = executionScratchSpace{
		chainConfig: chainConfig,
		db:          db,
		stateCache:  sdb,
		statedb:     statedb,
		gasConfig: gas.Config{
			MinPrice:                 params.GWei,
			MaxPerSecond:             maxGasPerChunk,
			ExcessConversionConstant: 20_000_000, // TODO(arr4n)
		},
		chunk: &chunk{ // pre-genesis
			// The below call to [executor.nextChunk] will use this as its
			// parent. The time will be incremented so it's ok if the -1
			// underflows here.
			timestamp: genesis.Timestamp - 1,
		},
	}

	running := make(chan struct{})
	go e.run(running)
	<-running

	// This is effectively a constructor for the scratch space's current chunk.
	// We call it, instead of constructing the chunk above, to ensure that
	// expected properties are in place for the first set of transactions.
	if err := e.nextChunk(ctx, &e.executeScratchSpace, nil /*overflowTx*/); err != nil {
		return err
	}

	b := e.chain.newBlock(genesisBlock)
	if err := b.Verify(ctx); err != nil {
		return err
	}
	return b.Accept(ctx)
}

func (e *executor) run(ready chan<- struct{}) {
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.chunks = sink.NewMonitor(make(map[uint64]*chunk))
	e.spawn(e.processQueue)

	close(ready)

	<-e.quit
	e.spawned.Wait()
	e.chunks.Close()
	e.queue.Close()
	snaps := e.snaps.Close()
	snaps.Disable()
	snaps.Release()

	close(e.done)
}

func (e *executor) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
}

// quitCtx returns a `Context`, derived from [context.Background], that is
// cancelled when [executor.quit] is closed.
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
		// Genesis, by definition, has no previous blocks but we still need to
		// trigger the chunk for its timestamp.
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

func (e *executor) processQueue() {
	ctx := e.quitCtx()

	for {
		block, err := sink.FromMonitor(ctx, e.queue,
			func(q *queue.FIFO[*Block]) bool {
				return q.Len() > 0
			},
			func(q *queue.FIFO[*Block]) (*Block, error) {
				return q.Pop(), nil
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
			return
		}

		switch err := e.execute(ctx, block); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
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
	chainConfig *params.ChainConfig
	db          ethdb.Database
	stateCache  state.Database
	statedb     *state.StateDB

	chunk     *chunk
	gasConfig gas.Config
	excess    gas.Gas
}

func (x *executionScratchSpace) chunkCapacity(c *chunk) gas.Gas {
	return c.donation + x.gasConfig.MaxPerSecond
}

func (x *executionScratchSpace) gasPrice() gas.Price {
	return gas.CalculatePrice(
		x.gasConfig.MinPrice,
		x.excess,
		x.gasConfig.ExcessConversionConstant,
	)
}

type chunk struct {
	// For use during execution.
	donation  gas.Gas // from the previous chunk
	consumed  gas.Gas
	timestamp uint64

	// Post-execution reporting.
	receipts      []*types.Receipt
	stateRootPost common.Hash
	// For use by the block builder. The reduction in excess is clipped to avoid
	// going below zero so can't be computed outside of the execution stream,
	// while the actual post-execution excess is to allow the builder to assert
	// invariants.
	excessReduction, excessPost gas.Gas

	filledBy time.Time // wall time for metrics
}

func (e *executor) execute(ctx context.Context, b *Block) error {
	x := &e.executeScratchSpace

	if bTime := b.b.Time(); bTime > x.chunk.timestamp {
		e.logger().Fatal(
			"BUG: chunk executing the future",
			zap.Uint64("block time", bTime),
			zap.Uint64("chunk time", x.chunk.timestamp),
		)
	}
	header := types.CopyHeader(b.b.Header())
	header.BaseFee = new(big.Int)

	e.logger().Debug(
		"Executing accepted block",
		zap.Uint64("height", b.Height()),
		zap.Uint64("timestamp", header.Time),
		zap.Int("transactions", len(b.b.Transactions())),
	)

	gasPool := core.GasPool(math.MaxUint64) // required by geth but irrelevant so max it out

	for ti, tx := range b.b.Transactions() {
		header.BaseFee.SetUint64(uint64(x.gasPrice()))
		x.statedb.SetTxContext(tx.Hash(), ti) // TODO(arr4n) `ti` is not correct here

		receipt, err := core.ApplyTransaction(
			x.chainConfig,
			e.chain,
			&header.Coinbase,
			&gasPool,
			x.statedb,
			header,
			tx,
			(*uint64)(&x.chunk.consumed),
			vm.Config{},
		)
		if err != nil {
			return fmt.Errorf("tx[%d]: %w", ti, err)
		}
		// TODO(arr4n) add tips here
		receipt.EffectiveGasPrice = new(big.Int).Set(header.BaseFee)

		x.excess += gas.Gas(receipt.GasUsed >> 1) // see ACP for details

		// Criterion (2) of the ACP is not met so continue filling.
		if x.chunk.consumed < x.chunkCapacity(x.chunk) {
			x.chunk.receipts = append(x.chunk.receipts, receipt)
			continue
		}

		// The overflowing tx is going to be in the next chunk so reduce the
		// current one's consumed gas.
		x.chunk.consumed -= gas.Gas(receipt.GasUsed)
		if err := e.nextChunk(ctx, x, receipt); err != nil {
			return err
		}
	}

	// Let C and B be the timestamps of the chunk and the block (header),
	// respectively:
	//
	// * B < C: execution is slower than blocks so the queue is not exhausted
	// * C == B: queue exhausted per criterion (1) of the ACP
	// * B > C: invalid execution of unknown future consensus (enforced earlier)
	if x.chunk.timestamp == header.Time {
		if err := e.nextChunk(ctx, x, nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *executor) nextChunk(ctx context.Context, x *executionScratchSpace, overflowTx *types.Receipt) error {
	prev := x.chunk
	x.chunk = nil // avoid accidentally using it before replacement

	if err := e.snaps.UseThenSignal(ctx, func(snaps *snapshot.Tree) error {
		// Commit() will call [snapshot.Tree.Cap] so we call it while holding
		// `snaps` even though we only explicitly use `snaps` until a few lines
		// down. Cue the Rustaceans.
		root, err := x.statedb.Commit(prev.timestamp, true)
		if err != nil {
			return fmt.Errorf("%T.Commit() at end of chunk at time %d: %w", x.statedb, prev.timestamp, err)
		}
		if err := x.stateCache.TrieDB().Commit(root, false); err != nil {
			return err
		}
		prev.stateRootPost = root
		prev.filledBy = time.Now()

		db, err := state.New(root, x.stateCache, snaps)
		if err != nil {
			return err
		}
		x.statedb = db
		return nil
	}); err != nil {
		return err
	}

	surplus, err := safemath.Sub(x.chunkCapacity(prev), prev.consumed)
	if err != nil {
		return fmt.Errorf("*BUG* over-filled chunk at timestamp %d consumed %d of %d gas", prev.timestamp, prev.consumed, x.chunkCapacity(prev))
	}
	x.chunk = &chunk{
		timestamp: prev.timestamp + 1,
	}
	curr := x.chunk

	if overflowTx != nil {
		// The queue wasn't exhausted so surplus gas is being used for the
		// overflowing transaction and MUST be donated to the next chunk but
		// MUST NOT count as a reduction in gas excess.
		curr.donation = surplus
		curr.consumed += gas.Gas(overflowTx.GasUsed)
		if x.chunkCapacity(curr) < gas.Gas(overflowTx.GasUsed) {
			e.logger().Fatal("unimplemented: empty interim chunk for XL tx")
		}
		curr.receipts = []*types.Receipt{overflowTx}
		// The execution loop updates `x.excess` after every tx, but the
		// overflowing one is being assigned to the next chunk.
		prev.excessPost = x.excess - gas.Gas(overflowTx.GasUsed)>>1
	} else {
		// Conversely, the queue was exhausted so the gas excess is reduced, but
		// we MUST NOT donate the surplus to the next chunk because the
		// execution stream is now dormant.
		old := x.excess
		x.excess = clippedSubtract(x.excess, surplus>>1)
		prev.excessReduction = old - x.excess
		prev.excessPost = x.excess
	}

	e.logger().Debug(
		"Concluding chunk",
		zap.Uint64("timestamp", prev.timestamp),
		zap.Int("receipts", len(prev.receipts)),
		zap.Bool("queue_exhausted", overflowTx == nil),
		zap.Uint64("gas_capacity", uint64(x.chunkCapacity(prev))),
		zap.Uint64("gas_consumed", uint64(prev.consumed)),
	)
	return e.chunks.UseThenSignal(e.quitCtx(), func(cs map[uint64]*chunk) error {
		cs[prev.timestamp] = prev
		return nil
	})
}
