package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
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
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"
	"golang.org/x/exp/constraints"

	"github.com/ava-labs/strevm/queue"
)

type executor struct {
	vm   *VM
	quit <-chan struct{}
	done chan<- struct{}

	spawned sync.WaitGroup

	chainConfig *params.ChainConfig
	gasClock    gasClock

	queue  sink.Monitor[*queue.FIFO[*Block]]
	chunks sink.Monitor[*executionResults]

	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
	snaps               sink.Monitor[*snapshot.Tree]
}

type executionResults struct {
	chunks   map[uint64]*chunk
	receipts map[common.Hash]*types.Receipt
}

// init initialises the executor and returns the genesis block, upon which the
// [Block.Verify] and then [Block.Accept] methods MUST be called once the
// [blockBuilder] is ready.
func (e *executor) init(ctx context.Context, genesis *core.Genesis) (*Block, error) {
	sdb := state.NewDatabase(e.vm.db)
	tdb := sdb.TrieDB()
	chainConfig, genesisHash, err := core.SetupGenesisBlock(e.vm.db, tdb, genesis)
	if err != nil {
		return nil, err
	}

	e.chainConfig = chainConfig
	e.gasClock = gasClock{
		time: genesis.Timestamp,
		config: gas.Config{
			MinPrice:                 params.GWei,
			MaxPerSecond:             maxGasPerChunk,
			TargetPerSecond:          maxGasPerChunk / 2,
			ExcessConversionConstant: 20_000_000, // TODO(arr4n)
		},
	}

	genesisBlock := rawdb.ReadBlock(e.vm.db, genesisHash, 0)
	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: false,
	}
	snaps, err := snapshot.New(snapConf, e.vm.db, tdb, genesisBlock.Root())
	if err != nil {
		return nil, err
	}
	e.snaps = sink.NewMonitor(snaps)
	statedb, err := state.New(genesisBlock.Root(), sdb, snaps)
	if err != nil {
		return nil, err
	}

	e.executeScratchSpace = executionScratchSpace{
		stateCache: sdb,
		statedb:    statedb,
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
		return nil, err
	}

	b := e.vm.newBlock(genesisBlock)
	b.accepted.Store(true)
	b.executed.Store(true)
	return b, nil

}

func (e *executor) run(ready chan<- struct{}) {
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.chunks = sink.NewMonitor(&executionResults{
		chunks:   make(map[uint64]*chunk),
		receipts: make(map[common.Hash]*types.Receipt),
	})
	e.spawn(e.processQueue)

	close(ready)

	<-e.quit
	e.spawned.Wait()
	e.chunks.Close()
	e.queue.Close()
	snaps := e.snaps.Close()
	snaps.Disable()
	snaps.Release()

	e.done <- struct{}{}
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
	return e.vm.snowCtx.Log
}

// enqueueAccepted is intended to be called by [Block.Accept], passing itself
// as the first argument. The `parent` MAY be nil i.f.f `block` is the genesis.
func (e *executor) enqueueAccepted(ctx context.Context, block, parent *Block) error {
	var blocks []*Block
	if block.NumberU64() == 0 {
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
		lastTime := parent.Time()
		gap := block.Time() - lastTime

		blocks = make([]*Block, gap)
		blocks[gap-1] = block

		for i := range uint64(gap - 1) {
			blocks[i] = &Block{
				Block: types.NewBlockWithHeader(&types.Header{
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
				zap.Uint64("timestamp", block.Time()),
				zap.Any("hash", block.Hash()),
			)
			return
		}
	}
}

type executionScratchSpace struct {
	stateCache state.Database
	statedb    *state.StateDB

	chunk  *chunk
	excess gas.Gas
}

func (e *executor) chunkCapacity(c *chunk) gas.Gas {
	return c.donation + e.gasClock.config.MaxPerSecond
}

func (e *executor) gasPrice(excess gas.Gas) gas.Price {
	return gas.CalculatePrice(
		e.gasClock.config.MinPrice,
		excess,
		e.gasClock.config.ExcessConversionConstant,
	)
}

type gasClock struct {
	time     uint64
	consumed gas.Gas // this second
	excess   gas.Gas

	config gas.Config
}

func (c gasClock) clone() gasClock {
	return c
}

func (c *gasClock) consume(g gas.Gas) {
	c.consumed += g
	c.time += uint64(c.consumed / c.config.MaxPerSecond)
	c.consumed %= c.config.MaxPerSecond

	// The ACP describes the increase in excess in terms of `p`, a rational
	// number, where R=pT. Substituting p for R/T, we get an increase of
	// g(R-T)/R.
	quo, _ := mulDiv(g, c.config.MaxPerSecond-c.config.TargetPerSecond, c.config.MaxPerSecond)
	c.excess += gas.Gas(quo)
}

func (c *gasClock) fastForward(to uint64) {
	if to <= c.time {
		return
	}

	surplus := c.config.MaxPerSecond - c.consumed
	surplus += gas.Gas(to-c.time-1) * c.config.MaxPerSecond // -1 avoids double-counting gas remaining this second
	// By similar reasoning to that in [gasClock.consume], we get a decrease in
	// excess of sT/R.
	quo, _ := mulDiv(surplus, c.config.TargetPerSecond, c.config.MaxPerSecond)
	c.excess = boundedSubtract(c.excess, quo, 0)

	c.time = to
	c.consumed = 0
}

func (c *gasClock) remainingGasThisSecond() gas.Gas {
	// TODO(arr4n) NB! this MUST be modified to account for ACP-176
	// functionality allowing validators to change the config.
	return c.config.MaxPerSecond - c.consumed
}

func mulDiv(a, b, c gas.Gas) (quo, rem gas.Gas) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, r := bits.Div64(hi, lo, uint64(c))
	return gas.Gas(q), gas.Gas(r)
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

func (c *chunk) isGenesis() bool {
	return c.timestamp == 0
}

func eqAndPrintOrPanic[T constraints.Integer](desc string, a, b T) {
	if a != b {
		panic(fmt.Sprintf("%s: %d != %d", desc, a, b))
	}
	// fmt.Println(desc, a, b)
}

func (e *executor) execute(ctx context.Context, b *Block) error {
	x := &e.executeScratchSpace

	e.gasClock.fastForward(b.Time())

	// This is a temporary check for equality between the gas-clock and
	// per-second-chunk approaches, which will be deleted when the latter is
	// removed.
	assertClockEquality := func() {
		eqAndPrintOrPanic("time", x.chunk.timestamp, e.gasClock.time)
		eqAndPrintOrPanic("excess", x.excess, e.gasClock.excess)
		eqAndPrintOrPanic("consumed", x.chunk.consumed-x.chunk.donation, e.gasClock.consumed)
	}
	assertClockEquality()

	if bTime := b.Time(); bTime > x.chunk.timestamp {
		e.logger().Fatal(
			"BUG: chunk executing the future",
			zap.Uint64("block time", bTime),
			zap.Uint64("chunk time", x.chunk.timestamp),
		)
	}
	header := types.CopyHeader(b.Header())
	header.BaseFee = new(big.Int)

	e.logger().Debug(
		"Executing accepted block",
		zap.Uint64("height", b.Height()),
		zap.Uint64("timestamp", header.Time),
		zap.Int("transactions", len(b.Transactions())),
	)

	gasPool := core.GasPool(math.MaxUint64) // required by geth but irrelevant so max it out

	for ti, tx := range b.Transactions() {
		header.BaseFee.SetUint64(uint64(e.gasPrice(x.excess)))
		x.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			e.chainConfig,
			e.vm,
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

		// TODO(arr4n) this results in rounding down excess relative to doing it
		// at the end of a block.
		x.excess += gas.Gas(receipt.GasUsed >> 1) // see ACP for details
		e.gasClock.consume(gas.Gas(receipt.GasUsed))

		// TODO(arr4n) add a receipt cache to the [executor] to allow API calls
		// to access them before the end of the block.
		b.execution.receipts = append(b.execution.receipts, receipt)
		// Criterion (2) of the ACP is not met so continue filling.
		if x.chunk.consumed < e.chunkCapacity(x.chunk) {
			x.chunk.receipts = append(x.chunk.receipts, receipt)
			continue
		}

		// The overflowing tx is going to be in the next chunk so reduce the
		// current one's consumed gas.
		x.chunk.consumed -= gas.Gas(receipt.GasUsed)
		if err := e.nextChunk(ctx, x, receipt); err != nil {
			return err
		}
		assertClockEquality()
	}

	b.execution.by = e.gasClock.clone()
	root, err := e.commitState(ctx, x, b.NumberU64())
	if err != nil {
		return err
	}
	b.execution.stateRootPost = root
	b.executed.Store(true)

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

func (e *executor) commitState(ctx context.Context, x *executionScratchSpace, blockNum uint64) (common.Hash, error) {
	var root common.Hash
	err := e.snaps.UseThenSignal(ctx, func(snaps *snapshot.Tree) error {
		// Commit() will call [snapshot.Tree.Cap] so we call it while holding
		// `snaps` even though we only explicitly use `snaps` until a few lines
		// down. Cue the Rustaceans.
		var err error
		root, err = x.statedb.Commit(blockNum, true)
		if err != nil {
			return fmt.Errorf("%T.Commit() at end of chunk at time %d: %w", x.statedb, blockNum, err)
		}
		if err := x.stateCache.TrieDB().Commit(root, false); err != nil {
			return err
		}

		db, err := state.New(root, x.stateCache, snaps)
		if err != nil {
			return err
		}
		x.statedb = db
		return nil
	})
	return root, err
}

func (e *executor) nextChunk(ctx context.Context, x *executionScratchSpace, overflowTx *types.Receipt) error {
	prev := x.chunk
	x.chunk = nil // avoid accidentally using it before replacement

	root, err := e.commitState(ctx, x, prev.timestamp)
	if err != nil {
		return err
	}
	prev.stateRootPost = root
	prev.filledBy = time.Now()

	surplus, err := safemath.Sub(e.chunkCapacity(prev), prev.consumed)
	if err != nil {
		return fmt.Errorf("*BUG* over-filled chunk at timestamp %d consumed %d of %d gas", prev.timestamp, prev.consumed, e.chunkCapacity(prev))
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
		if e.chunkCapacity(curr) < gas.Gas(overflowTx.GasUsed) {
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
		zap.Uint64("gas_capacity", uint64(e.chunkCapacity(prev))),
		zap.Uint64("gas_consumed", uint64(prev.consumed)),
	)
	return e.chunks.UseThenSignal(e.quitCtx(), func(res *executionResults) error {
		res.chunks[prev.timestamp] = prev
		for _, r := range prev.receipts {
			res.receipts[r.TxHash] = r
		}
		return nil
	})
}
