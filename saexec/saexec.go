// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saexec provides the execution module of [Streaming Asynchronous
// Execution] (SAE).
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saexec

import (
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
)

// An Executor accepts and executes a [blocks.Block] FIFO queue.
type Executor struct {
	quit, done chan struct{}
	log        logging.Logger
	hooks      hook.Points

	gasClock *gastime.Time
	queue    chan *blocks.Block

	lastEnqueued, lastExecuted atomic.Pointer[blocks.Block]

	enqueueEvents event.FeedOf[*types.Block]
	headEvents    event.FeedOf[core.ChainHeadEvent]
	chainEvents   event.FeedOf[core.ChainEvent]
	logEvents     event.FeedOf[[]*types.Log]

	chainContext core.ChainContext
	chainConfig  *params.ChainConfig
	db           ethdb.Database
	stateCache   state.Database
	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [Executor.init], [Executor.execute], and [Executor.Close].
	executeScratchSpace executionScratchSpace
}

// New constructs and starts a new [Executor]. Call [Executor.Close] to release
// resources created by this constructor.
//
// The last-executed block MAY be the genesis block for an always-SAE chain, the
// last pre-SAE synchronous block during transition, or the last asynchronously
// executed block after shutdown and recovery.
func New(
	lastExecuted *blocks.Block,
	blockSrc BlockSource,
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	triedbConfig *triedb.Config,
	hooks hook.Points,
	log logging.Logger,
) (*Executor, error) {
	e := &Executor{
		quit:         make(chan struct{}), // closed by [Executor.Close]
		done:         make(chan struct{}), // closed by [Executor.processQueue] after `quit` is closed
		log:          log,
		hooks:        hooks,
		queue:        make(chan *blocks.Block, 4096), // arbitrarily sized
		chainContext: &chainContext{blockSrc, log},
		chainConfig:  chainConfig,
		db:           db,
		stateCache:   state.NewDatabaseWithConfig(db, triedbConfig),
	}
	e.lastEnqueued.Store(lastExecuted)
	e.lastExecuted.Store(lastExecuted)
	if err := e.init(); err != nil {
		return nil, err
	}

	go e.processQueue()
	return e, nil
}

func (e *Executor) init() error {
	last := e.lastExecuted.Load()
	e.gasClock = last.ExecutedByGasTime().Clone()

	root := last.PostExecutionStateRoot()
	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, e.db, e.stateCache.TrieDB(), root)
	if err != nil {
		return err
	}
	statedb, err := state.New(root, e.stateCache, snaps)
	if err != nil {
		return err
	}

	e.executeScratchSpace = executionScratchSpace{
		snaps:   snaps,
		statedb: statedb,
	}
	return nil
}

// Close shuts down the [Executor], waits for the currently executing block
// to complete, and then releases all resources.
func (e *Executor) Close() {
	close(e.quit)
	<-e.done

	snaps := e.executeScratchSpace.snaps
	snaps.Disable()
	snaps.Release()
}

// ChainConfig returns the config originally passed to [New].
func (e *Executor) ChainConfig() *params.ChainConfig {
	return e.chainConfig
}

// StateCache returns caching database underpinning execution.
func (e *Executor) StateCache() state.Database {
	return e.stateCache
}

// LastExecuted returns the last-executed block in a threadsafe manner.
func (e *Executor) LastExecuted() *blocks.Block {
	return e.lastExecuted.Load()
}

// LastEnqueued returns the last-enqueued block in a threadsafe manner.
func (e *Executor) LastEnqueued() *blocks.Block {
	return e.lastEnqueued.Load()
}
