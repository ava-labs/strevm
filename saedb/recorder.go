// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saedb

import (
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/libevm/triedb"
	"go.uber.org/zap"
)

// Config allows parameterization of the TrieDB and when
// state is committed.
type Config struct {
	// TODO(alarso16): move minimal elements to config and construct in method.
	TrieDBConfig *triedb.Config
	Archival     bool // if true, will store every state on disk
}

// SnapshotCacheSizeMB is the snapshot cache size used by the executor.
// TODO(alarso16): move to config
const SnapshotCacheSizeMB = 128

var _ StateDBOpener = (*Recorder)(nil)

// Recorder provides an abstraction to all state-related operations of the executor.
// It manages all database operations not exposed by the [state.StateDB] itself.
//
// All methods are safe to be called even after [Recorder.Close], but state
// will be unavailable.
type Recorder struct {
	snaps       *snapshot.Tree
	cache       state.Database
	isHashDB    bool
	isArchival  bool
	log         logging.Logger
	currentRoot common.Hash
}

// NewRecorder provides a new [Recorder] on the underlying database
func NewRecorder(db ethdb.Database, c Config, lastExecuted common.Hash, log logging.Logger) (*Recorder, error) {
	cache := state.NewDatabaseWithConfig(db, c.TrieDBConfig)
	_, isHashDB := cache.TrieDB().Backend().(triedb.HashDB)
	snapConf := snapshot.Config{
		CacheSize:  SnapshotCacheSizeMB,
		AsyncBuild: true,
	}
	snaps, err := snapshot.New(snapConf, db, cache.TrieDB(), lastExecuted)
	if err != nil {
		return nil, err
	}
	return &Recorder{
		snaps:       snaps,
		cache:       cache,
		currentRoot: lastExecuted,
		isHashDB:    isHashDB,
		isArchival:  c.Archival,
		log:         log,
	}, nil
}

// OnExecution tracks the root and may commit the trie associated with the root
// to the database if [ShouldCommitTrieDB] returns true, or the [Config]
// specifies that the node is archival.
//
// This state will be available until [Recorder.ReleaseInMemory] has been called
// or the root as many times as it has been referenced.
//
// Note: Snapshot memory leaks are avoided internally by [state.StateDB.Commit].
func (r *Recorder) OnExecution(root common.Hash, height uint64) error {
	// Because `Release` is always expected to be called (whether the state root changed or not)
	// We must always add an additional reference
	r.reference(root) // keepalive until dereference
	r.currentRoot = root

	if !r.isArchival && !ShouldCommitTrieDB(height) {
		return nil
	}

	tdb := r.cache.TrieDB()
	if err := tdb.Commit(root, false /* log */); err != nil {
		return fmt.Errorf("%T.Commit(%#x) at end of block %d: %v", tdb, root, height, err)
	}
	return nil
}

func (r *Recorder) reference(root common.Hash) {
	if !r.isHashDB {
		return
	}

	// Never returns an error.
	if err := r.cache.TrieDB().Reference(root, common.Hash{}); err != nil {
		log.Error("*triedb.Database.Reference()", zap.Error(err))
	}
}

// ReleaseInMemory informs the recorder that the state corresponding
// with `root` can have its reference count reduced. If the reference
// count is 0, the state will be removed from memory.
//
// This should be called on each block after its state is no longer
// needed. If the state is already on disk, no operation is performed.
func (r *Recorder) ReleaseInMemory(root common.Hash) {
	if !r.isHashDB {
		return
	}

	// Never returns an error.
	if err := r.cache.TrieDB().Dereference(root); err != nil {
		log.Error("*triedb.Database.Dereference()", zap.Error(err))
	}
}

// StateDB provides a [state.StateDB] at the given root.
//
// Each [state.StateDB] can be constructed and used concurrently.
// However, the right to call [state.StateDB.Commit] is reserved
// for canonical blocks, as any other use could result in a memory
// leak or state corruption.
func (r *Recorder) StateDB(root common.Hash) (*state.StateDB, error) {
	return state.New(root, r.cache, r.snaps)
}

// Close commits the most recent state to the database for shutdown.
func (r *Recorder) Close() (errs error) {
	defer func() {
		r.snaps.Release()
		if err := r.cache.TrieDB().Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("triedb.Database.Close(): %v", err))
		}
	}()

	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ensure there are changes.
	if r.currentRoot != r.snaps.DiskRoot() {
		if err := r.snaps.Cap(r.currentRoot, 0); err != nil {
			errs = errors.Join(errs, fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err))
		}
	}

	// If we have new state, commit changes to database for easier startup.
	// If there's no changes, this is a no-op.
	if err := r.cache.TrieDB().Commit(r.currentRoot, true /* log */); err != nil {
		errs = errors.Join(errs, fmt.Errorf("triedb.Database.Commit() for %#x: %v", r.currentRoot, err))
	}
	return errs
}
