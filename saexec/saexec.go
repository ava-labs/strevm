// Package saexec provides the execution module of [Streaming Asynchronous
// Execution] (SAE).
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saexec

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/queue"
)

// An Executor accepts and executes a [blocks.Block] FIFO queue.
type Executor struct {
	quit  <-chan struct{}
	log   logging.Logger
	hooks hook.Points

	spawned sync.WaitGroup

	gasClock     gastime.Time
	queue        sink.Monitor[*queue.FIFO[*blocks.Block]]
	lastExecuted *atomic.Pointer[blocks.Block] // shared with VM

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

// New constructs a new [Executor]. The last-executed block MUST NOT be modified
// by code outside of the [Executor]; for an always-SAE chain it MAY be a
// genesis block.
//
// The returned [Executor] is not active until [Executor.Run] is called.
func New(
	lastExecuted *atomic.Pointer[blocks.Block],
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	hooks hook.Points,
	log logging.Logger,
) (*Executor, error) {
	e := &Executor{
		lastExecuted: lastExecuted,
		chainConfig:  chainConfig,
		db:           db,
		hooks:        hooks,
		log:          log,
	}
	if err := e.init(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Executor) init() error {
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
	e.gasClock = *last.ExecutedByGasTime().Clone()
	return nil
}

// Run starts a goroutine to process blocks passed to
// [Executor.EnqueueAccepted], closes the `ready` channel to signal that calling
// of said method can begin, and blocks until the `quit` channel is closed.
//
// After Run returns, the [Executor] is no longer in a valid state for executing
// future blocks. The `quit` channel SHOULD therefore only be closed on
// shutdown.
func (e *Executor) Run(quit <-chan struct{}, ready chan<- struct{}) {
	e.quit = quit
	e.queue = sink.NewMonitor(new(queue.FIFO[*blocks.Block]))
	e.spawn(e.processQueue)

	close(ready)

	<-e.quit
	e.spawned.Wait()
	e.queue.Close()

	snaps := e.executeScratchSpace.snaps
	snaps.Disable()
	snaps.Release()
}

func (e *Executor) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
}

// quitCtx returns a `Context`, derived from [context.Background], that is
// cancelled when [executor.quit] is closed.
func (e *Executor) quitCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	e.spawn(func() {
		<-e.quit
		cancel()
	})
	return ctx
}

// ChainConfig returns the config originally passed to [New].
func (e *Executor) ChainConfig() *params.ChainConfig {
	return e.chainConfig
}

// StateCache returns caching database underpinning execution.
func (e *Executor) StateCache() state.Database {
	return e.stateCache
}
