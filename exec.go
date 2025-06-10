package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
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
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/queue"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

type executor struct {
	quit  <-chan struct{}
	log   logging.Logger
	hooks Hooks

	spawned sync.WaitGroup

	gasClock     gastime.Time
	queue        sink.Monitor[*queue.FIFO[*Block]]
	lastExecuted *atomic.Pointer[Block] // shared with VM

	// During bootstrapping we need to know when the queue has been executed to
	// avoid verification returning an error due to waiting on the execution
	// stream for settlement. This [sink.Gate] is only valid if *not* waited
	// upon concurrently with a call to [VM.AcceptBlock] as that may result in a
	// spurious unblocking with a non-empty queue.
	queueCleared sink.Gate

	headEvents  event.FeedOf[core.ChainHeadEvent]
	chainEvents event.FeedOf[core.ChainEvent]
	logEvents   event.FeedOf[[]*types.Log]

	chainConfig *params.ChainConfig
	db          ethdb.Database
	stateCache  state.Database
	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executionScratchSpace
}

func (e *executor) init() error {
	last := e.lastExecuted.Load()
	root := last.Root()

	e.stateCache = state.NewDatabase(e.db)
	sdb := e.stateCache
	tdb := sdb.TrieDB()

	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, e.db, tdb, root)
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
	e.gasClock = *last.execution.by.Clone()
	return nil
}

func (e *executor) run(ready chan<- struct{}) {
	e.queue = sink.NewMonitor(new(queue.FIFO[*Block]))
	e.queueCleared = sink.NewGate()
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

// enqueueAccepted is intended to be called by [Block.Accept], passing itself
// as the argument.
func (e *executor) enqueueAccepted(ctx context.Context, block *Block) error {
	return e.queue.UseThenSignal(ctx, func(q *queue.FIFO[*Block]) error {
		q.Push(block)
		e.queueCleared.Block()
		return nil
	})
}

func (e *executor) processQueue() {
	ctx := e.quitCtx()

	for {
		type pop struct {
			block      *Block
			emptyAfter bool
		}

		popped, err := sink.FromMonitor(ctx, e.queue,
			func(q *queue.FIFO[*Block]) bool {
				return q.Len() > 0
			},
			func(q *queue.FIFO[*Block]) (pop, error) {
				b := q.Pop()
				return pop{
					block:      b,
					emptyAfter: q.Len() == 0,
				}, nil
			},
		)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			// [sink.Monitor.Wait] will only return the [context.Context] error
			// or the error returned by its argument, so this is theoretically
			// impossible but included for completeness to be detected in tests.
			e.log.Fatal("BUG: popping from queue", zap.Error(err))
			return
		}

		block := popped.block
		switch err := e.execute(ctx, block); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			e.log.Fatal(
				"Executing accepted block",
				zap.Error(err),
				zap.Uint64("height", block.Height()),
				zap.Uint64("timestamp", block.Time()),
				zap.Any("hash", block.Hash()),
			)
			return
		}

		// This may race with a concurrent call to [VM.AcceptBlock], but that is
		// documented and also acceptable as we only ever Wait() inside
		// [VM.AcceptBlock].
		if popped.emptyAfter {
			e.queueCleared.Open()
		}
	}
}

type executionScratchSpace struct {
	snaps   *snapshot.Tree
	statedb *state.StateDB
}

type executionResults struct {
	by       gastime.Time `canoto:"value,1"`
	byTime   time.Time
	receipts types.Receipts

	gasUsed       gas.Gas     `canoto:"uint,2"`
	receiptRoot   common.Hash `canoto:"fixed bytes,3"`
	stateRootPost common.Hash `canoto:"fixed bytes,4"`

	canotoData canotoData_executionResults `canoto:"nocopy"`
}

func (e *executor) execute(ctx context.Context, b *Block) error {
	x := &e.executeScratchSpace

	// If [VM.AcceptBlock] returns an error after enqueuing the block, we would
	// receive the same block twice for execution should consensus retry
	// acceptance.
	if last, curr := e.lastExecuted.Load().Height(), b.Height(); curr != last+1 {
		return fmt.Errorf("executing blocks out of order: %d then %d", last, curr)
	}

	hook.BeforeBlock(&e.gasClock, b.Header(), e.hooks.GasTarget(b.parent.Block))

	header := types.CopyHeader(b.Header())
	header.BaseFee = e.gasClock.BaseFee().ToBig()
	e.log.Debug(
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
			chainContext{},
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

		// TODO(arr4n) add a receipt cache to the [executor] to allow API calls
		// to access them before the end of the block.
		receipts[ti] = receipt
	}
	endTime := time.Now()
	hook.AfterBlock(&e.gasClock, blockGasConsumed)

	root, err := e.commitState(ctx, x, b.NumberU64())
	if err != nil {
		return err
	}
	b.execution = &executionResults{
		by:            *e.gasClock.Clone(),
		byTime:        endTime,
		receipts:      receipts,
		gasUsed:       blockGasConsumed,
		receiptRoot:   types.DeriveSha(receipts, trieHasher()),
		stateRootPost: root,
	}

	batch := e.db.NewBatch()
	rawdb.WriteHeadBlockHash(batch, b.Hash())
	rawdb.WriteHeadHeaderHash(batch, b.Hash())
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

	e.sendPostExecutionEvents(b.Block, receipts)
	b.executed.Store(true)
	e.lastExecuted.Store(b)

	e.log.Debug(
		"Block execution complete",
		zap.Uint64("height", b.Height()),
		zap.Time("gas_time", e.gasClock.AsTime()),
		zap.Time("wall_time", endTime),
		zap.Int("tx_count", len(b.Transactions())),
	)
	return nil
}

func (e *executor) sendPostExecutionEvents(b *types.Block, receipts types.Receipts) {
	e.headEvents.Send(core.ChainHeadEvent{Block: b})

	var logs []*types.Log
	for _, r := range receipts {
		logs = append(logs, r.Logs...)
	}
	e.chainEvents.Send(core.ChainEvent{
		Block: b,
		Hash:  b.Hash(),
		Logs:  logs,
	})
	e.logEvents.Send(logs)
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
