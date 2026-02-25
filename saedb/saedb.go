// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saedb provides functionality related to storage and access of
// [Streaming Asynchronous Execution] data.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saedb

import (
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/utils/buffer"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/triedb"
	"go.uber.org/zap"
)

const (
	// CommitTrieDBEvery is the number of blocks between commits of the state
	// trie to disk.
	CommitTrieDBEvery     = 1 << commitTrieDBEveryLog2
	commitTrieDBEveryLog2 = 12
	commitTrieDBMask      = CommitTrieDBEvery - 1
)

// ShouldCommitTrieDB returns whether or not to commit the state trie to disk.
func ShouldCommitTrieDB(blockNum uint64) bool {
	return blockNum&commitTrieDBMask == 0
}

// LastCommittedTrieDBHeight returns the largest value <= the argument at which
// [ShouldCommitTrieDB] would have returned true.
func LastCommittedTrieDBHeight(atOrBefore uint64) uint64 {
	return atOrBefore &^ commitTrieDBMask
}

// ExecutionResults provides type safety for a [database.HeightIndex], to be
// used for persistence of SAE-specific execution results, avoiding possible
// collision with `rawdb` keys.
type ExecutionResults struct {
	database.HeightIndex
}

// StateRecorder provides an abstraction to deciding if/when to persist a state
// to the database or dereference it, making it unavailable and uncommittable.
type StateRecorder struct {
	// snaps is owned by [Executor]. It may be mutated during
	// [Executor.execute] and [Executor.Close]. Callers MUST treat
	// values returned from [Executor.SnapshotTree] as read-only.
	//
	// [snapshot.Tree] is safe for concurrent read access - for example,
	// blockchain_reader.go exposes bc.snaps without holding any lock:
	// https://github.com/ava-labs/libevm/blob/312fa380513e/core/blockchain_reader.go#L356-L367
	snaps    *snapshot.Tree
	cache    state.Database
	inMemory buffer.Queue[common.Hash]
}

const (
	// SnapshotCacheSizeMB is the snapshot cache size used by the executor.
	SnapshotCacheSizeMB = 128
	// StateHistory is the number of recent states available in memory.
	StateHistory = 32
)

// NewStateRecorder returns a [StateRecorder] object that abstracts database interactions on the EVM state.
//
// TODO(alarso16): Provide a custom config to generate the [triedb.Config].
func NewStateRecorder(db ethdb.Database, c *triedb.Config, lastExecuted common.Hash, log logging.Logger) (*StateRecorder, error) {
	cache := state.NewDatabaseWithConfig(db, c)
	q, err := buffer.NewBoundedQueue(StateHistory, func(root common.Hash) {
		// This error doesn't need to be fatal, since all current state is likely still valid.
		// However, it may cause a memory leak
		if err := cache.TrieDB().Dereference(root); err != nil {
			log.Error("Dereferencing old root from memory", zap.Stringer("root", root), zap.Error(err))
		}
	})

	if err != nil {
		return nil, err
	}

	snapConf := snapshot.Config{
		CacheSize:  SnapshotCacheSizeMB,
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, db, cache.TrieDB(), lastExecuted)
	if err != nil {
		return nil, err
	}
	return &StateRecorder{
		snaps:    snaps,
		cache:    cache,
		inMemory: q,
	}, nil
}

// Close commits the most recent state to the database for shutdown.
func (e *StateRecorder) Close() error {
	root, _ := e.inMemory.Index(e.inMemory.Len() - 1)
	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ignore it.
	if root != e.snaps.DiskRoot() {
		if err := e.snaps.Cap(root, 0); err != nil {
			return fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err)
		}
	}
	e.snaps.Release()

	err := e.cache.TrieDB().Commit(root, true /* log */)
	return errors.Join(err, e.cache.TrieDB().Close())
}

// StateCache returns caching database underpinning execution.
func (e *StateRecorder) StateCache() state.Database {
	return e.cache
}

// SnapshotTree returns the snapshot tree, which MUST only be used for reading.
func (e *StateRecorder) SnapshotTree() *snapshot.Tree {
	return e.snaps
}

// StateDB provides a [state.StateDB] at the given root.
func (e *StateRecorder) StateDB(root common.Hash) (*state.StateDB, error) {
	return state.New(root, e.cache, e.snaps)
}

// Record tracks the root and may commit the trie associated with the root
// to the database if the height is on an multiple of [CommitTrieDBEvery].
func (e *StateRecorder) Record(root common.Hash, height uint64) error {
	e.inMemory.Push(root)
	if !ShouldCommitTrieDB(height) {
		return nil
	}

	tdb := e.cache.TrieDB()
	if err := tdb.Commit(root, false /* log */); err != nil {
		return fmt.Errorf("%T.Commit(%#x) at end of block %d: %v", tdb, root, height, err)
	}
	return nil
}
