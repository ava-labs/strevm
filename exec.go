package sae

import (
	"context"
	"encoding/binary"
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

	"github.com/ava-labs/strevm/acp176"
	"github.com/ava-labs/strevm/queue"
)

//go:generate canoto exec.go

type executor struct {
	vm   *VM
	quit <-chan struct{}

	spawned sync.WaitGroup

	chainConfig *params.ChainConfig
	gasClock    gasClock

	queue sink.Monitor[*queue.FIFO[*Block]]

	stateCache state.Database
	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
}

// init initialises the executor based of the async genesis, which may be either
// an actual genesis block or the last synchronous block for a chain upgrading
// to SAE.
func (e *executor) init(db ethdb.Database, genesis *Block) error {
	e.stateCache = state.NewDatabase(db)

	root := genesis.Root()
	sdb := e.stateCache
	tdb := sdb.TrieDB()

	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, db, tdb, root)
	if err != nil {
		return err
	}

	statedb, err := state.New(root, sdb, snaps)
	if err != nil {
		return err
	}
	e.executeScratchSpace = executionScratchSpace{
		snaps:   snaps,
		statedb: statedb,
	}

	e.gasClock = gasClock{
		time: genesis.Time(),
		state: acp176.State{
			Gas: gas.State{
				Excess: 0,
			},
			TargetExcess: acp176.DesiredTargetExcess(maxGasPerSecond / acp176.TargetToMax),
		},
	}
	return nil
}

func (e *executor) run(ready chan<- struct{}) {
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.spawn(e.processQueue)

	close(ready)

	<-e.quit
	e.spawned.Wait()
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

type executionResults struct {
	by       gasClock `canoto:"value,1"`
	byTime   time.Time
	receipts types.Receipts

	gasUsed       gas.Gas     `canoto:"fint64,2"`
	receiptRoot   common.Hash `canoto:"fixed bytes,3"`
	stateRootPost common.Hash `canoto:"fixed bytes,4"`

	canotoData canotoData_executionResults
}

type gasClock struct {
	time     uint64  `canoto:"fint64,1"`
	consumed gas.Gas `canoto:"fint64,2"` // this second

	state acp176.State

	canotoData canotoData_gasClock `canoto:"noatomic"`
}

type gasParams struct {
	T, R gas.Gas
}

func (c gasClock) clone() gasClock {
	return c
}

func (c *gasClock) gasPrice() gas.Price {
	return c.state.GasPrice()
}

func (c *gasClock) params() gasParams {
	T := c.state.Target()
	return gasParams{
		R: 2 * T,
		T: T,
	}
}

func (c *gasClock) asTime() time.Time {
	nsec, _ /*remainder*/ := mulDiv(c.consumed, c.params().R, 1e9)
	return time.Unix(int64(c.time), int64(nsec))
}

func (c *gasClock) consume(g gas.Gas) {
	params := c.params()

	c.consumed += g
	c.time += uint64(c.consumed / params.R)
	c.consumed %= params.R

	// The ACP describes the increase in excess in terms of `p`, a rational
	// number, where R=pT. Substituting p for R/T, we get an increase of
	// g(R-T)/R.
	quo, _ := mulDiv(g, params.R-params.T, params.R)
	c.state.Gas.Excess += gas.Gas(quo)
}

func (c *gasClock) fastForward(to uint64) {
	if to <= c.time {
		return
	}

	params := c.params()
	surplus := params.R - c.consumed
	surplus += gas.Gas(to-c.time-1) * params.R // -1 avoids double-counting gas remaining this second
	// By similar reasoning to that in [gasClock.consume], we get a decrease in
	// excess of sT/R.
	quo, _ := mulDiv(surplus, params.T, params.R)
	c.state.Gas.Excess = boundedSubtract(c.state.Gas.Excess, quo, 0)

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
	return gas.Gas(timestamp-c.time)*c.params().R - c.consumed, true
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
	var blockGasConsumed gas.Gas

	receipts := make(types.Receipts, len(b.Transactions()))
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
			(*uint64)(&blockGasConsumed),
			vm.Config{},
		)
		if err != nil {
			return fmt.Errorf("tx[%d]: %w", ti, err)
		}
		// TODO(arr4n) add tips here
		receipt.EffectiveGasPrice = new(big.Int).Set(header.BaseFee)

		// TODO(arr4n) add a receipt cache to the [executor] to allow API calls
		// to access them before the end of the block.
		receipts[ti] = receipt
	}
	endTime := time.Now()
	e.gasClock.consume(blockGasConsumed)

	root, err := e.commitState(ctx, x, b.NumberU64())
	if err != nil {
		return err
	}
	b.execution = &executionResults{
		by:            e.gasClock.clone(),
		byTime:        endTime,
		receipts:      receipts,
		gasUsed:       blockGasConsumed,
		receiptRoot:   types.DeriveSha(receipts, trieHasher()),
		stateRootPost: root,
	}

	batch := e.vm.db.NewBatch()
	rawdb.WriteHeadBlockHash(batch, b.Hash())
	// TODO(arr4n) move writing of receipts into settlement, where the
	// associated state root is committed on the trie DB. For now it's done here
	// to support immediate eth_getTransactionReceipt as the API treats
	// last-executed as the "latest" block and the upstream API implementation
	// requires this write.
	rawdb.WriteReceipts(batch, b.Hash(), b.NumberU64(), receipts)
	if err := b.writePostExecutionState(batch); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}

	b.executed.Store(true)
	e.vm.last.executed.Store(b)

	e.logger().Debug(
		"Block execution complete",
		zap.Uint64("height", b.Height()),
		zap.Time("gas_time", e.gasClock.asTime()),
		zap.Time("wall_time", endTime),
		zap.Int("tx_count", len(b.Transactions())),
	)
	return nil
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

func (b *Block) writePostExecutionState(w ethdb.KeyValueWriter) error {
	return w.Put(execResultsDBKey(b.NumberU64()), b.execution.MarshalCanoto())
}

func (vm *VM) readPostExecutionState(blockNum uint64) (*executionResults, error) {
	buf, err := vm.db.Get(execResultsDBKey(blockNum))
	if err != nil {
		return nil, err
	}
	r := new(executionResults)
	if err := r.UnmarshalCanoto(buf); err != nil {
		return nil, err
	}
	return r, nil
}

func execResultsDBKey(blockNum uint64) []byte {
	key := []byte("sae-post-exec-12345678")
	binary.BigEndian.PutUint64(key[14:], blockNum)
	return key
}
