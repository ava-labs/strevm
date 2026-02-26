// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/utils/buffer"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/worstcase"
)

const (
	// SnapshotCacheSizeMB is the snapshot cache size used by the executor.
	SnapshotCacheSizeMB = 128
	// StateHistory is the number of recent states available in memory.
	StateHistory = 32
)

// stateRecorder provides an abstraction to all state-related operations of the executor.
// It manages all database operations not exposed by the [state.StateDB] itself.
type stateRecorder struct {
	snaps    *snapshot.Tree
	cache    state.Database
	inMemory buffer.Queue[common.Hash]
}

// TODO(alarso16): Provide a custom config to generate the [triedb.Config].
func newStateRecorder(db ethdb.Database, c *triedb.Config, lastExecuted common.Hash, log logging.Logger) (*stateRecorder, error) {
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
	q.Push(lastExecuted)

	snapConf := snapshot.Config{
		CacheSize:  SnapshotCacheSizeMB,
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, db, cache.TrieDB(), lastExecuted)
	if err != nil {
		return nil, err
	}
	return &stateRecorder{
		snaps:    snaps,
		cache:    cache,
		inMemory: q,
	}, nil
}

// close commits the most recent state to the database for shutdown.
func (s *stateRecorder) close() (errs error) {
	// Always release resources
	defer func() {
		s.snaps.Release()
		if err := s.cache.TrieDB().Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("triedb.Database.Close(): %v", err))
		}
	}()

	// If we have new state, attempt to commit changes to database for easier startup.
	root, ok := s.inMemory.Index(s.inMemory.Len() - 1)
	if !ok {
		return nil // buffer empty
	}

	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ensure there are changes.
	if root != s.snaps.DiskRoot() {
		if err := s.snaps.Cap(root, 0); err != nil {
			errs = errors.Join(errs, fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err))
		}
	}

	if err := s.cache.TrieDB().Commit(root, true /* log */); err != nil {
		errs = errors.Join(errs, fmt.Errorf("triedb.Database.Commit() for %#x: %v", root, err))
	}
	return errs
}

// WorstCaseState returns a [worstcase.State] at the starting at the provided settled block.
func (s *stateRecorder) WorstCaseState(hooks hook.Points, config *params.ChainConfig, settled *blocks.Block) (*worstcase.State, error) {
	return worstcase.NewState(hooks, config, s.cache, settled, s.snaps)
}

// StateDB provides a [state.StateDB] at the given root.
//
// Although this can be called concurrently, it is only safe for concurrent reading.
// [state.StateDB.Commit] (i.e. writes) cannot occur concurrently.
// Any commit calls can only be done by the [Executor].
func (s *stateRecorder) StateDB(root common.Hash) (*state.StateDB, error) {
	return state.New(root, s.cache, s.snaps)
}

// record tracks the root and may commit the trie associated with the root
// to the database if the height is on an multiple of [CommitTrieDBEvery].
//
// Note: Snapshot memory leaks are avoided internally by [state.StateDB.Commit].
func (s *stateRecorder) record(root common.Hash, height uint64) error {
	// Push root if unique - don't want to remove an in-use state!
	mostRecent, ok := s.inMemory.Index(s.inMemory.Len() - 1)
	if !ok {
		return errors.New("no known state roots")
	}
	if mostRecent != root {
		s.inMemory.Push(root)
	}

	if !saedb.ShouldCommitTrieDB(height) {
		return nil
	}

	tdb := s.cache.TrieDB()
	if err := tdb.Commit(root, false /* log */); err != nil {
		return fmt.Errorf("%T.Commit(%#x) at end of block %d: %v", tdb, root, height, err)
	}
	return nil
}
