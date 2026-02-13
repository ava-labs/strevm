// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"
	"iter"
	"math"
	"slices"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/ethdb"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saexec"
)

type recovery struct {
	db              ethdb.Database
	log             logging.Logger
	config          Config
	lastSynchronous *blocks.Block
}

func (rec *recovery) newCanonicalBlock(num uint64, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	ethB, err := canonicalBlock(rec.db, num)
	if err != nil {
		return nil, err
	}
	return blocks.New(ethB, parent, lastSettled, rec.log)
}

func (rec *recovery) lastBlockWithStateRootAvailable() (*blocks.Block, error) {
	num := saedb.LastCommittedTrieDBHeight(
		rawdb.ReadHeadHeader(rec.db).Number.Uint64(),
	)
	if num <= rec.lastSynchronous.NumberU64() {
		return rec.lastSynchronous, nil
	}

	b, err := rec.newCanonicalBlock(num, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := b.RestoreExecutionArtefacts(rec.db); err != nil {
		return nil, err
	}
	{
		// This would require the node to crash at such a precise point in time
		// that it's not worth a preemptive fix. If this ever occurs then just
		// try the root [params.CommitTrieDBEvery] blocks earlier.
		root := b.PostExecutionStateRoot()
		if _, err := state.NewDatabaseWithConfig(rec.db, rec.config.TrieDBConfig).OpenTrie(root); err != nil {
			return nil, fmt.Errorf("database corrupted: latest expected state root (block %d / %#x) unavailable: %v", b.NumberU64(), b.Hash(), err)
		}
	}
	return b, nil
}

// recoverFromDB returns the block to be used as the `lastExecuted` argument to
// [saexec.New], along with an iterator of blocks to pass to
// [saexec.Executor.Enqueue]. Enqueuing of all blocks and waiting for the last
// one to be executed will leave the [saexec.Executor] in almost the same state
// as before shutdown. [VM.rebuildBlocksInMemory] MUST then be called to fully
// reinstate internal invariants.
func (rec *recovery) recoverFromDB() (*blocks.Block, iter.Seq2[*blocks.Block, error], error) {
	var _ = saexec.New // protect the import to allow comment linking

	execAfter, err := rec.lastBlockWithStateRootAvailable()
	if err != nil {
		return nil, nil, err
	}
	toExecute, _ := rawdb.ReadAllCanonicalHashes(rec.db, execAfter.NumberU64()+1, math.MaxUint64, math.MaxInt)

	return execAfter, func(yield func(*blocks.Block, error) bool) {
		parent := execAfter
		for _, num := range toExecute {
			b, err := rec.newCanonicalBlock(num, parent, nil)
			if !yield(b, err) || err != nil {
				return
			}
			parent = b
		}
	}, nil
}

// lastOf returns the lastOf element in a slice, which MUST NOT be empty.
func lastOf[E any](s []E) E {
	return s[len(s)-1]
}

// rebuildBlocksInMemory returns a block-hash-keyed map of all blocks from the
// last executed back to, and including, the block that it settled. It returns
// said settled block separately, for convenience.
func (rec *recovery) rebuildBlocksInMemory(lastExecuted *blocks.Block) (_ *syncMap[common.Hash, *blocks.Block], lastSettled *blocks.Block, _ error) {
	chain := []*blocks.Block{lastExecuted} // reverse height order
	blackhole := new(atomic.Pointer[blocks.Block])

	// extend appends to the chain all the blocks in settler's ancestry up to
	// and including the block that it settled.
	extend := func(settler *blocks.Block) error {
		settleAt := blocks.PreciseTime(rec.config.Hooks, settler.Header()).Add(-params.Tau)
		tm := proxytime.Of[gas.Gas](settleAt)

		for extended := false; ; extended = true {
			switch b := lastOf(chain); {
			case b.Synchronous():
				return nil

			case b.ExecutedByGasTime().Compare(tm) <= 0:
				if !extended {
					return nil
				}
				return b.MarkSettled(blackhole)

			case b.Height() == rec.lastSynchronous.Height()+1:
				chain = append(chain, rec.lastSynchronous)

			default:
				parent, err := rec.newCanonicalBlock(b.Height()-1, nil, nil)
				if err != nil {
					return err
				}
				if err := parent.RestoreExecutionArtefacts(rec.db); err != nil {
					return err
				}
				chain = append(chain, parent)
			}
		}
	}

	if err := extend(lastExecuted); err != nil {
		return nil, nil, err
	}
	// The chain has been extended back by exactly the number of blocks needed
	// to restore the [lastSettled, head] range. It will be extended further as
	// we find interim last-settled blocks, so snapshot it.
	restore := slices.Clone(chain)

	bMap := newSyncMap[common.Hash, *blocks.Block]()
	for i, b := range restore {
		bMap.Store(b.Hash(), b)
		if i+1 == len(restore) {
			break
		}
		if err := extend(b); err != nil {
			return nil, nil, err
		}
		if err := b.SetAncestors(restore[i+1], lastOf(chain)); err != nil {
			return nil, nil, err
		}
	}
	return bMap, lastOf(restore), nil
}
