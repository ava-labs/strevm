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
	"github.com/ava-labs/libevm/triedb/hashdb"
	"go.uber.org/zap"
)

const (
	// DefaultSnapshotCacheSizeMB is the snapshot cache size used by the executor.
	DefaultSnapshotCacheSizeMB = 128
	// DefaultTrieDBCacheSizeMB is the default cache size for trie nodes.
	DefaultTrieDBCacheSizeMB = 512
)

// Config allows parameterization of the TrieDB and when
// state is committed.
type Config struct {
	Archival             bool // whether to store every state on disk
	SnapshotCacheSizeMB  int  // Default: [DefaultSnapshotCacheSizeMB]. Set < 0 to disable snapshot.
	TrieDBCacheSizeBytes int  // Default: [DefaultTrieDBCacheSizeMB]. Set < 0 to disable cache.
}

// TrieDBConfig returns a config that can be used to create a [triedb.Database] based on
// the [Config] parameters provided.
//
// All [triedb.Database] must be closed, unless the TrieDB cache is disabled, as this can
// result in a memory leak.
func (c Config) TrieDBConfig() *triedb.Config {
	if c.TrieDBCacheSizeBytes <= 0 {
		return triedb.HashDefaults
	}

	return &triedb.Config{
		HashDB: &hashdb.Config{
			CleanCacheSize: c.TrieDBCacheSizeBytes,
		},
	}
}

func (c Config) snapConfig() *snapshot.Config {
	size := DefaultSnapshotCacheSizeMB
	switch {
	case c.SnapshotCacheSizeMB < 0:
		return nil
	case c.SnapshotCacheSizeMB > 0:
		size = c.SnapshotCacheSizeMB
	}

	return &snapshot.Config{
		CacheSize:  size,
		AsyncBuild: true,
	}
}

var _ StateDBOpener = (*Tracker)(nil)

// Tracker provides an abstraction to state-related operations, managing all
// database operations not exposed by the [state.StateDB] itself.
//
// All methods are safe to be called even after [Tracker.Close], but state
// will be unavailable.
type Tracker struct {
	snaps       *snapshot.Tree
	cache       state.Database
	isArchival  bool
	log         logging.Logger
	currentRoot common.Hash
}

// NewTracker provides a new [Tracker] on the underlying database.
func NewTracker(db ethdb.Database, c Config, lastExecuted common.Hash, log logging.Logger) (*Tracker, error) {
	cache := state.NewDatabaseWithConfig(db, c.TrieDBConfig())
	_, isHashDB := cache.TrieDB().Backend().(triedb.HashDB)
	if !isHashDB {
		return nil, fmt.Errorf("unsupported DB: %T", cache.TrieDB().Backend())
	}
	var snaps *snapshot.Tree
	if snapConf := c.snapConfig(); snapConf != nil {
		var err error
		snaps, err = snapshot.New(*snapConf, db, cache.TrieDB(), lastExecuted)
		if err != nil {
			return nil, err
		}
	}
	return &Tracker{
		snaps:       snaps,
		cache:       cache,
		currentRoot: lastExecuted,
		isArchival:  c.Archival,
		log:         log,
	}, nil
}

// Track tracks the root and may commit the trie associated with the root
// to the database if [ShouldCommitTrieDB] returns true, or the [Config]
// specifies that the node is archival.
//
// This state will be available in memory until [Tracker.Untrack] has been
// called for the root as many times as [Tracker.Track] has been called.
func (t *Tracker) Track(root common.Hash) {
	// Because [Tracker.Untrack] is always expected to be called (whether the state root changed or not),
	// we must always add an additional reference
	t.reference(root) // keepalive until dereference
	t.currentRoot = root
}

func (t *Tracker) reference(root common.Hash) {
	// Never returns an error because it's [triedb.HashDB].
	if err := t.cache.TrieDB().Reference(root, common.Hash{}); err != nil {
		log.Error("*triedb.Database.Reference()", zap.Error(err))
	}
}

// CheckCommit uses the provided height to decide if any root needs committed, following
// the below rules (in order):
//
// 1. If [Config.Archival] is true, then `executionRoot` will be committed.
// 2. If [ShouldCommitTrieDB] based on `height`, `settledRoot` is committed.
// 3. Otherwise, nothing is committed.
//
// This does NOT change in-memory tracking.
func (t *Tracker) CheckCommit(settledRoot, executionRoot common.Hash, height uint64) error {
	tdb := t.cache.TrieDB()
	if t.isArchival {
		if err := tdb.Commit(executionRoot, false /* log */); err != nil {
			return fmt.Errorf("%T.Commit(%#x) post-execution at end of block %d :%v", tdb, executionRoot, height, err)
		}
		return nil
	}

	if !ShouldCommitTrieDB(height) {
		return nil
	}

	if err := tdb.Commit(settledRoot, false /* log */); err != nil {
		return fmt.Errorf("%T.Commit(%#x) settled at end of block %d: %v", tdb, settledRoot, height, err)
	}
	return nil
}

// Untrack informs the [Tracker] that the state corresponding
// with `root` can have its reference count reduced. If the reference
// count is 0, the state will be removed from memory.
//
// This should be called on each block after its state is no longer
// needed. If the state is already on disk, no operation is performed.
func (t *Tracker) Untrack(root common.Hash) {
	// Never returns an error because it's [triedb.HashDB].
	if err := t.cache.TrieDB().Dereference(root); err != nil {
		log.Error("*triedb.Database.Dereference()", zap.Error(err))
	}
}

// StateDB provides a [state.StateDB] at the given root.
//
// Each [state.StateDB] can be constructed and used concurrently.
// However, the right to call [state.StateDB.Commit] is reserved
// for canonical blocks, as any other use could result in a memory
// leak or state corruption.
func (t *Tracker) StateDB(root common.Hash) (*state.StateDB, error) {
	return state.New(root, t.cache, t.snaps)
}

// Close commits the most recent state to the database for shutdown.
func (t *Tracker) Close() (errs error) {
	defer func() {
		if err := t.cache.TrieDB().Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("triedb.Database.Close(): %v", err))
		}
	}()

	// If we have new state, commit changes to database for easier startup.
	// If there's no changes, this is a no-op.
	if err := t.cache.TrieDB().Commit(t.currentRoot, true /* log */); err != nil {
		errs = errors.Join(errs, fmt.Errorf("triedb.Database.Commit() for %#x: %v", t.currentRoot, err))
	}

	if t.snaps == nil {
		return errs
	}

	defer t.snaps.Release()
	// We don't use [snapshot.Tree.Journal] because re-orgs are impossible under
	// SAE so we don't mind flattening all snapshot layers to disk. Note that
	// calling `Cap([disk root], 0)` returns an error when it's actually a
	// no-op, so we ensure there are changes.
	if t.currentRoot != t.snaps.DiskRoot() {
		if err := t.snaps.Cap(t.currentRoot, 0); err != nil {
			errs = errors.Join(errs, fmt.Errorf("snapshot.Tree.Cap([last post-execution state root], 0): %v", err))
		}
	}

	return errs
}
