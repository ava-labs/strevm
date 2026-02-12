// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"
	"iter"
	"math"
	"slices"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saexec"
)

func (vm *VM) lastBlockWithStateRootAvailable(lastSync *blocks.Block) (*blocks.Block, error) {
	lastExec := rawdb.ReadHeadHeader(vm.db)
	num := max(
		lastSync.NumberU64(),
		params.LastCommittedTrieDBHeight(lastExec.Number.Uint64()),
	)
	if num == lastSync.NumberU64() {
		return lastSync, nil
	}

	b, err := vm.newBlock(vm.canonicalBlock(num), nil, nil)
	if err != nil {
		return nil, err
	}
	if err := b.RestoreExecutionArtefacts(vm.db); err != nil {
		return nil, err
	}
	{
		// This would require the node to crash at such a precise point in time
		// that it's not worth a preemptive fix. If this ever occurs then just
		// try the root [params.CommitTrieDBEvery] blocks earlier.
		root := b.PostExecutionStateRoot()
		if _, err := state.NewDatabaseWithConfig(vm.db, vm.config.TrieDBConfig).OpenTrie(root); err != nil {
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
func (vm *VM) recoverFromDB(lastSync *blocks.Block) (*blocks.Block, iter.Seq2[*blocks.Block, error], error) {
	var _ = saexec.New // protect the import to allow comment linking

	execAfter, err := vm.lastBlockWithStateRootAvailable(lastSync)
	if err != nil {
		return nil, nil, err
	}
	toExecute, _ := rawdb.ReadAllCanonicalHashes(vm.db, execAfter.NumberU64()+1, math.MaxUint64, math.MaxInt)

	return execAfter, func(yield func(*blocks.Block, error) bool) {
		parent := execAfter
		for _, num := range toExecute {
			b, err := blocks.New(vm.canonicalBlock(num), parent, nil, vm.log())
			if !yield(b, err) || err != nil {
				return
			}
			parent = b
		}
	}, nil
}

// rebuildBlocksInMemory reinstates internal invariants pertaining to blocks
// kept in memory. After it returns, all blocks from the
// [saexec.Executor.LastExecuted] back to the block that it settled will be in
// memory, with their ancestry populated as required by invariants.
func (vm *VM) rebuildBlocksInMemory(lastSynchronous *blocks.Block) error {
	head := vm.exec.LastExecuted()
	chain := []*blocks.Block{head} // reverse height order
	blackhole := new(atomic.Pointer[blocks.Block])

	extend := func(settler *blocks.Block) error {
		settleAt := blocks.PreciseTime(vm.hooks(), settler.Header()).Add(-params.Tau)
		tm := proxytime.Of[gas.Gas](settleAt)

		for extended := false; ; extended = true {
			switch b := chain[len(chain)-1]; {
			case b.Synchronous():
				return nil

			case b.ExecutedByGasTime().Compare(tm) <= 0:
				if !extended {
					return nil
				}
				return b.MarkSettled(blackhole)

			case b.Height() == lastSynchronous.Height()+1:
				chain = append(chain, lastSynchronous)

			default:
				parent, err := vm.newBlock(vm.canonicalBlock(b.Height()-1), nil, nil)
				if err != nil {
					return err
				}
				if err := parent.RestoreExecutionArtefacts(vm.db); err != nil {
					return err
				}
				chain = append(chain, parent)
			}
		}
	}

	if err := extend(head); err != nil {
		return err
	}
	// The chain has been extended back by exactly the number of blocks needed
	// to restore the [lastSettled, head] range. It will be extended further as
	// we find interim last-settled blocks, so snapshot it.
	restore := slices.Clone(chain)

	for i, b := range restore {
		vm.blocks.Store(b.Hash(), b)

		switch i {
		case len(restore) - 1:
			vm.last.settled.Store(b)

		default:
			if err := extend(b); err != nil {
				return err
			}
			if err := b.SetAncestors(restore[i+1], chain[len(chain)-1]); err != nil {
				return err
			}
		}
	}

	vm.last.accepted.Store(head)
	vm.preference.Store(head)

	return nil
}
