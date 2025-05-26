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
	vm   *VM
	quit <-chan struct{}

	spawned sync.WaitGroup

	chainConfig *params.ChainConfig
	gasClock    gasClock

	queue    sink.Monitor[*queue.FIFO[*Block]]
	receipts sink.Monitor[map[common.Hash]*types.Receipt] // TODO(arr4n) make this an LRU, or similar.

	stateCache state.Database
	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
}

// init initialises the executor and returns the genesis block, which MUST be
// added to the VM's set of accepted blocks.
func (e *executor) init(ctx context.Context, genesis *core.Genesis, db ethdb.Database) (*Block, error) {
	e.stateCache = state.NewDatabase(db)

	sdb := e.stateCache
	tdb := sdb.TrieDB()
	chainConfig, genesisHash, err := core.SetupGenesisBlock(e.vm.db, tdb, genesis)
	if err != nil {
		return nil, err
	}

	e.chainConfig = chainConfig
	e.gasClock = gasClock{
		time: genesis.Timestamp,
		config: gasConfig{
			minPrice:                 params.GWei,
			capPerSecond:             maxGasPerSecond,
			targetPerSecond:          maxGasPerSecond / 2,
			excessConversionConstant: 20_000_000, // TODO(arr4n)
		},
	}

	genesisBlock := rawdb.ReadBlock(e.vm.db, genesisHash, 0)
	stateRoot := genesisBlock.Root()

	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: false,
	}
	snaps, err := snapshot.New(snapConf, e.vm.db, tdb, stateRoot)
	if err != nil {
		return nil, err
	}
	statedb, err := state.New(stateRoot, sdb, snaps)
	if err != nil {
		return nil, err
	}
	e.executeScratchSpace = executionScratchSpace{
		snaps:   snaps,
		statedb: statedb,
	}

	b := e.vm.newBlock(genesisBlock)
	b.accepted.Store(true)
	b.execution = &executionResults{
		by:            gasClock{time: genesisBlock.Time()},
		stateRootPost: genesisBlock.Root(),
	}
	b.executed.Store(true)
	return b, nil
}

func (e *executor) run(ready chan<- struct{}) {
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.receipts = sink.NewMonitor(make(map[common.Hash]*types.Receipt))
	e.spawn(e.processQueue)

	close(ready)

	<-e.quit
	e.spawned.Wait()
	e.receipts.Close()
	e.queue.Close()

	snaps := e.executeScratchSpace.snaps
	snaps.Disable()
	snaps.Release()
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
// as the argument.
func (e *executor) enqueueAccepted(ctx context.Context, block *Block) error {
	return e.queue.UseThenSignal(ctx, func(q *queue.FIFO[*Block]) error {
		q.Push(block)
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
	snaps   *snapshot.Tree
	statedb *state.StateDB
}

type (
	gasClock struct {
		config gasConfig

		time     uint64
		consumed gas.Gas // this second
		excess   gas.Gas
	}

	gasConfig struct {
		capPerSecond, targetPerSecond gas.Gas   // R, T
		minPrice                      gas.Price // M
		excessConversionConstant      gas.Gas   // K
	}
)

func (c gasClock) clone() gasClock {
	return c
}

func (c *gasClock) gasPrice() gas.Price {
	return gas.CalculatePrice(
		c.config.minPrice,
		c.excess,
		c.config.excessConversionConstant,
	)
}

func (c *gasClock) asTime() time.Time {
	nsec, _ /*remainder*/ := mulDiv(c.consumed, c.config.capPerSecond, 1e9)
	return time.Unix(int64(c.time), int64(nsec))
}

func (c *gasClock) consume(g gas.Gas) {
	c.consumed += g
	c.time += uint64(c.consumed / c.config.capPerSecond)
	c.consumed %= c.config.capPerSecond

	// The ACP describes the increase in excess in terms of `p`, a rational
	// number, where R=pT. Substituting p for R/T, we get an increase of
	// g(R-T)/R.
	quo, _ := mulDiv(g, c.config.capPerSecond-c.config.targetPerSecond, c.config.capPerSecond)
	c.excess += gas.Gas(quo)
}

func (c *gasClock) fastForward(to uint64) {
	if to <= c.time {
		return
	}

	surplus := c.config.capPerSecond - c.consumed
	surplus += gas.Gas(to-c.time-1) * c.config.capPerSecond // -1 avoids double-counting gas remaining this second
	// By similar reasoning to that in [gasClock.consume], we get a decrease in
	// excess of sT/R.
	quo, _ := mulDiv(surplus, c.config.targetPerSecond, c.config.capPerSecond)
	c.excess = boundedSubtract(c.excess, quo, 0)

	c.time = to
	c.consumed = 0
}

func (c *gasClock) after(timestamp uint64) bool {
	return timestamp < c.time || (timestamp == c.time && c.consumed > 0)
}

// remainingGasUntil returns the amount of gas that `c` would need to consume to
// reach `timestamp`, and a boolean indicating whether the timestamp is in the
// future relative to the clock.
func (c *gasClock) remainingGasUntil(timestamp uint64) (_ gas.Gas, isFuture bool) {
	// TODO(arr4n) NB! this MUST be modified to account for ACP-176
	// functionality allowing validators to change the config.
	if timestamp == c.time && c.consumed == 0 {
		return 0, true
	}
	if timestamp <= c.time {
		return 0, false
	}
	return gas.Gas(timestamp-c.time)*c.config.capPerSecond - c.consumed, true
}

func mulDiv(a, b, c gas.Gas) (quo, rem gas.Gas) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, r := bits.Div64(hi, lo, uint64(c))
	return gas.Gas(q), gas.Gas(r)
}

func (e *executor) execute(ctx context.Context, b *Block) error {
	x := &e.executeScratchSpace

	e.gasClock.fastForward(b.Time())

	header := types.CopyHeader(b.Header())
	// TODO(arr4n) set the gas price during block building and just check it here.
	header.BaseFee = new(big.Int).SetUint64(uint64(e.gasClock.gasPrice()))
	e.logger().Debug(
		"Executing accepted block",
		zap.Uint64("height", b.Height()),
		zap.Uint64("timestamp", header.Time),
		zap.Int("transactions", len(b.Transactions())),
	)

	gasPool := core.GasPool(math.MaxUint64) // required by geth but irrelevant so max it out
	var blockGasConsumed uint64

	receipts := make([]*types.Receipt, len(b.Transactions()))
	for ti, tx := range b.Transactions() {
		x.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			e.chainConfig,
			e.vm,
			&header.Coinbase,
			&gasPool,
			x.statedb,
			header,
			tx,
			&blockGasConsumed,
			vm.Config{},
		)
		if err != nil {
			return fmt.Errorf("tx[%d]: %w", ti, err)
		}
		// TODO(arr4n) add tips here
		receipt.EffectiveGasPrice = new(big.Int).Set(header.BaseFee)

		// TODO(arr4n) this results in rounding down excess relative to doing it
		// at the end of a block.
		e.gasClock.consume(gas.Gas(receipt.GasUsed))
		// TODO(arr4n) add a receipt cache to the [executor] to allow API calls
		// to access them before the end of the block.
		receipts[ti] = receipt
	}
	endTime := time.Now()

	root, err := e.commitState(ctx, x, b.NumberU64())
	if err != nil {
		return err
	}
	b.execution = &executionResults{
		by:            e.gasClock.clone(),
		byTime:        endTime,
		receipts:      receipts,
		stateRootPost: root,
	}
	b.executed.Store(true)

	e.logger().Debug(
		"Block execution complete",
		zap.Uint64("height", b.Height()),
		zap.Time("gas_time", e.gasClock.asTime()),
		zap.Time("wall_time", endTime),
		zap.Int("tx_count", len(b.Transactions())),
	)

	return e.receipts.UseThenSignal(e.quitCtx(), func(rs map[common.Hash]*types.Receipt) error {
		for _, r := range b.execution.receipts {
			rs[r.TxHash] = r
		}
		return nil
	})
}

func (e *executor) commitState(ctx context.Context, x *executionScratchSpace, blockNum uint64) (common.Hash, error) {
	root, err := x.statedb.Commit(blockNum, true)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%T.Commit() at end of block %d: %w", x.statedb, blockNum, err)
	}

	db, err := state.New(root, e.stateCache, x.snaps)
	if err != nil {
		return common.Hash{}, err
	}
	x.statedb = db
	return root, nil
}
