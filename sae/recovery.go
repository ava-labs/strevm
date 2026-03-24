// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"iter"
	"math"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saexec"
)

type recovery struct {
	db              ethdb.Database
	xdb             saedb.ExecutionResults
	chainConfig     *params.ChainConfig
	log             logging.Logger
	hooks           hook.Points
	config          Config
	lastSynchronous *blocks.Block
}

func (rec *recovery) newCanonicalBlock(num uint64, parent *blocks.Block) (*blocks.Block, error) {
	ethB, err := canonicalBlock(rec.db, num)
	if err != nil {
		return nil, err
	}
	return blocks.New(ethB, parent, nil, rec.log)
}

// findLastCommitted uses the database to find the last expected state, and
// returns the block with that post-execution state.
func (rec *recovery) findLastCommitted() (*blocks.Block, error) {
	// most recently executed block number before shutdown
	num := rawdb.ReadHeadHeader(rec.db).Number.Uint64()
	if num <= rec.lastSynchronous.NumberU64() {
		return rec.lastSynchronous, nil
	}

	// If the node is not archival, then each block at height [saedb.CommitTrieDBEvery]
	// will have its settled state available.
	if !rec.config.DBConfig.Archival {
		// executionHeight represents the last block with the settled state available.
		executionHeight := saedb.LastCommittedTrieDBHeight(num)
		if executionHeight <= rec.lastSynchronous.NumberU64() {
			return rec.lastSynchronous, nil
		}
		b, err := rec.newCanonicalBlock(executionHeight, nil)
		if err != nil {
			return nil, err
		}

		num = rec.hooks.SettledHeight(b.Header())
	}

	settled, err := rec.newCanonicalBlock(num, nil)
	if err != nil {
		return nil, err
	}
	if err := settled.RestoreExecutionArtefacts(rec.db, rec.xdb, rec.chainConfig); err != nil {
		return nil, err
	}
	{
		// TODO(alarso16): When Firewood is added, the exact root may be unknown.
		sdb := state.NewDatabaseWithConfig(rec.db, rec.config.DBConfig.TrieDBConfig)
		root := settled.PostExecutionStateRoot()
		if _, err := sdb.OpenTrie(root); err != nil {
			return nil, fmt.Errorf(
				"database corrupted: checking for state root (block %d / %#x): %v",
				settled.NumberU64(), settled.Hash(), err,
			)
		}
	}
	return settled, nil
}

// canonicalAfter returns an iterator over all canonical blocks after `start`.
func (rec *recovery) canonicalAfter(start *blocks.Block) iter.Seq2[*blocks.Block, error] {
	toExecute, _ := rawdb.ReadAllCanonicalHashes(rec.db, start.NumberU64()+1, math.MaxUint64, math.MaxInt)

	return func(yield func(*blocks.Block, error) bool) {
		parent := start
		for _, num := range toExecute {
			b, err := rec.newCanonicalBlock(num, parent)
			if !yield(b, err) || err != nil {
				return
			}
			parent = b
		}
	}
}

// executeCritical executes all canonical blocks after `exec`'s last executed state.
// The map returned stores all blocks in the inclusive range between`lastSettled`
// and the `exec`'s new last executed block.
// All post-execution states in that range are guaranteed to be accessible.
func (rec *recovery) executeCritical(ctx context.Context, exec *saexec.Executor) (_ *syncMap[common.Hash, *blocks.Block], lastSettled *blocks.Block, _ error) {
	start := exec.LastExecuted()
	for b, err := range rec.canonicalAfter(start) {
		if err != nil {
			return nil, nil, err
		}
		if err := exec.Enqueue(ctx, b); err != nil {
			return nil, nil, err
		}
		// Avoid race with untracking by ensuring state is available.
		if err := b.WaitUntilExecuted(ctx); err != nil {
			return nil, nil, err
		}
	}

	// post-execution state between `start` and last settled isn't needed by consensus.
	// All remaining state will be appropriately tracked in [recovery.rebuildBlocksInMemory]
	// since the post-execution state was tracked by the executor.
	tr := exec.Tracker
	lastExecuted := exec.LastExecuted()
	settledHeight := rec.hooks.SettledHeight(lastExecuted.Header())
	for b := lastExecuted; b.NumberU64() > start.NumberU64(); b = b.ParentBlock() {
		if b.NumberU64() < settledHeight {
			tr.Untrack(b.PostExecutionStateRoot())
		}
	}

	return rec.rebuildBlocksInMemory(lastExecuted, tr)
}

// lastOf returns the lastOf element in a slice, which MUST NOT be empty.
func lastOf[E any](s []E) E {
	return s[len(s)-1]
}

// rebuildBlocksInMemory returns a block-hash-keyed map of all blocks from the
// last executed back to, and including, the block that it settled. It returns
// said settled block separately, for convenience.
func (rec *recovery) rebuildBlocksInMemory(lastExecuted *blocks.Block, tracker *saedb.Tracker) (_ *syncMap[common.Hash, *blocks.Block], lastSettled *blocks.Block, _ error) {
	chain := []*blocks.Block{lastExecuted} // reverse height order
	blackhole := new(atomic.Pointer[blocks.Block])

	// extend appends to the chain all the blocks in settler's ancestry up to
	// and including the block that it settled.
	extend := func(settler *blocks.Block) error {
		settleAt := blocks.PreciseTime(rec.hooks, settler.Header()).Add(-saeparams.Tau)
		tm := proxytime.Of[gas.Gas](settleAt)

		for {
			switch b := lastOf(chain); {
			case b.Synchronous():
				return nil

			case b.ExecutedByGasTime().Compare(tm) <= 0:
				if b.Settled() {
					return nil
				}
				return b.MarkSettled(blackhole)

			case b.Height() == rec.lastSynchronous.Height()+1:
				chain = append(chain, rec.lastSynchronous)

			default:
				parent, err := rec.newCanonicalBlock(b.Height()-1, nil)
				if err != nil {
					return err
				}
				if err := parent.RestoreExecutionArtefacts(rec.db, rec.xdb, rec.chainConfig); err != nil {
					return err
				}
				chain = append(chain, parent)

				if !b.Settled() {
					continue
				}
				if err := parent.MarkSettled(blackhole); err != nil {
					return err
				}
			}
		}
	}

	if err := extend(lastExecuted); err != nil {
		return nil, nil, err
	}
	lastSettled = lastOf(chain)
	bMap := newSyncMap[common.Hash, *blocks.Block](
		func(b *blocks.Block) {
			tracker.Track(b.SettledStateRoot())
			// Post-execution root not yet known.
		},
		func(b *blocks.Block) {
			tracker.Untrack(b.SettledStateRoot())
			if b.Executed() {
				tracker.Untrack(b.PostExecutionStateRoot())
			}
		},
	)
	for _, b := range chain {
		// `rec` already tracked all post-execution state roots for blocks that belong in this map,
		// and deleted all others in [recovery.executeCritical].
		bMap.Store(b.Hash(), b)
	}

	for i, b := range chain[:len(chain)-1] {
		if err := extend(b); err != nil {
			return nil, nil, err
		}
		if err := b.SetAncestors(chain[i+1], lastOf(chain)); err != nil {
			return nil, nil, err
		}
	}
	for _, b := range bMap.m {
		stage := blocks.Executed
		if b.Hash() == lastSettled.Hash() {
			stage = blocks.Settled
		}
		if err := b.CheckInvariants(stage); err != nil {
			return nil, nil, err
		}
	}
	return bMap, lastSettled, nil
}
