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
	"github.com/ava-labs/libevm/triedb"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/saedb"
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
	snaps           *snapshot.Tree
	cache           state.Database
	referencedRoots buffer.Queue[common.Hash]
}

// TODO(alarso16): Provide a custom config to generate the [triedb.Config].
func newStateRecorder(db ethdb.Database, c *triedb.Config, lastExecuted common.Hash, log logging.Logger) (*stateRecorder, error) {
	cache := state.NewDatabaseWithConfig(db, c)
	_, isHashDB := cache.TrieDB().Backend().(triedb.HashDB)
	q, err := buffer.NewBoundedQueue(StateHistory, func(root common.Hash) {
		if !isHashDB {
			return
		}
		if err := cache.TrieDB().Dereference(root); err != nil {
			log.Error("(*triedb.Database).Dereference()", zap.Stringer("root", root), zap.Error(err))
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
		snaps:           snaps,
		cache:           cache,
		referencedRoots: q,
	}, nil
}

// record tracks the root and may commit the trie associated with the root
// to the database if [saedb.ShouldCommitTrieDB] returns true.
//
// Note: Snapshot memory leaks are avoided internally by [state.StateDB.Commit].
func (s *stateRecorder) record(root common.Hash, height uint64) error {
	// State roots can only repeat with empty blocks, so we know that if this
	// root already is tracked, then it must be the most recent one.
	if s.lastRoot() != root {
		s.referencedRoots.Push(root)
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

// Returns the most recently recorded state root.
func (s *stateRecorder) lastRoot() common.Hash {
	r := s.referencedRoots
	h, _ := r.Index(r.Len() - 1) // invariant: always at least the last-executed
	return h
}

// StateDB provides a [state.StateDB] at the given root.
//
// Each [state.StateDB] can be constructed and used concurrently.
// However, the right to call [state.StateDB.Commit] is reserved
// by the [Executor], as any other use could result in a memory
// leak or state corruption.
func (s *stateRecorder) StateDB(root common.Hash) (*state.StateDB, error) {
	return state.New(root, s.cache, s.snaps)
}

// close commits the most recent state to the database for shutdown.
func (s *stateRecorder) close() (errs error) {
	defer func() {
		s.snaps.Release()
		if err := s.cache.TrieDB().Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("triedb.Database.Close(): %v", err))
		}
	}()

	root := s.lastRoot()

	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ensure there are changes.
	if root != s.snaps.DiskRoot() {
		if err := s.snaps.Cap(root, 0); err != nil {
			errs = errors.Join(errs, fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err))
		}
	}

	// If we have new state, commit changes to database for easier startup.
	if err := s.cache.TrieDB().Commit(root, true /* log */); err != nil {
		errs = errors.Join(errs, fmt.Errorf("triedb.Database.Commit() for %#x: %v", root, err))
	}
	return errs
}
