// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saexec provides the execution module of [Streaming Asynchronous
// Execution] (SAE).
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saexec

import (
	"fmt"
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
	"github.com/ava-labs/strevm/hook"
)

// An Executor accepts and executes a [blocks.Block] FIFO queue.
type Executor struct {
	quit, done chan struct{}
	log        logging.Logger
	hooks      hook.Points

	queue                      chan *blocks.Block
	lastEnqueued, lastExecuted atomic.Pointer[blocks.Block]

	enqueueEvents event.FeedOf[*types.Block]
	headEvents    event.FeedOf[core.ChainHeadEvent]
	chainEvents   event.FeedOf[core.ChainEvent]
	logEvents     event.FeedOf[[]*types.Log]

	chainContext core.ChainContext
	chainConfig  *params.ChainConfig
	db           ethdb.Database
	stateCache   state.Database
	// snaps MUST NOT be accessed by any methods other than [Executor.execute]
	// and [Executor.Close].
	snaps *snapshot.Tree
}

// New constructs and starts a new [Executor]. Call [Executor.Close] to release
// resources created by this constructor.
//
// The last-executed block MAY be the genesis block for an always-SAE chain, the
// last pre-SAE synchronous block during transition, or the last asynchronously
// executed block after shutdown and recovery.
func New(
	lastExecuted *blocks.Block,
	blockSrc blocks.Source,
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	triedbConfig *triedb.Config,
	hooks hook.Points,
	log logging.Logger,
) (*Executor, error) {
	cache := state.NewDatabaseWithConfig(db, triedbConfig)
	snapConf := snapshot.Config{
		CacheSize:  128, // MB
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, db, cache.TrieDB(), lastExecuted.PostExecutionStateRoot())
	if err != nil {
		return nil, err
	}

	e := &Executor{
		quit:         make(chan struct{}), // closed by [Executor.Close]
		done:         make(chan struct{}), // closed by [Executor.processQueue] after `quit` is closed
		log:          log,
		hooks:        hooks,
		queue:        make(chan *blocks.Block, 4096), // arbitrarily sized
		chainContext: &chainContext{blockSrc, log},
		chainConfig:  chainConfig,
		db:           db,
		stateCache:   cache,
		snaps:        snaps,
	}
	e.lastEnqueued.Store(lastExecuted)
	e.lastExecuted.Store(lastExecuted)

	go e.processQueue()
	return e, nil
}

// Close shuts down the [Executor], waits for the currently executing block
// to complete, and then releases all resources.
func (e *Executor) Close() error {
	close(e.quit)
	<-e.done

	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ignore it.
	if root := e.LastExecuted().PostExecutionStateRoot(); root != e.snaps.DiskRoot() {
		if err := e.snaps.Cap(root, 0); err != nil {
			return fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err)
		}
	}

	e.snaps.Release()
	return nil
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
