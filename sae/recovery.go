// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"iter"
	"math"
	"slices"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/proxytime"
)

func (vm *VM) lastBlockWithStateRootAvailable(lastSync *blocks.Block) (*blocks.Block, error) {
	lastExec := rawdb.ReadHeadHeader(vm.db)
	num := max(
		lastSync.NumberU64(),
		params.LastCommitedTrieDBHeight(lastExec.Number.Uint64()),
	)
	if num == lastSync.NumberU64() {
		return lastSync, nil
	}

	b, err := vm.newBlock(vm.canonicalBlock(num), nil, nil)
	if err != nil {
		return nil, err
	}
	if err := b.ReloadExecutionResults(vm.db); err != nil {
		return nil, err
	}
	return b, nil
}

func (vm *VM) recoverFromDB(lastSync *blocks.Block) (*blocks.Block, iter.Seq2[*blocks.Block, error], error) {
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

func (vm *VM) rebuildBlocksInMemory() error {
	head := vm.exec.LastExecuted()
	chain := []*blocks.Block{head}

	extend := func(settler *blocks.Block) error {
		settleAt := blocks.PreciseTime(vm.hooks(), settler.Header()).Add(-params.Tau)
		tm := proxytime.Of[gas.Gas](settleAt)
		blackhole := new(atomic.Pointer[blocks.Block])

		for extended := false; ; extended = true {
			b := chain[len(chain)-1]
			if b.Synchronous() {
				return nil
			}
			n := b.NumberU64()
			if n == 0 || b.ExecutedByGasTime().Compare(tm) <= 0 {
				if !extended {
					return nil
				}
				return b.MarkSettled(blackhole)
			}
			parent, err := vm.newBlock(vm.canonicalBlock(n-1), nil, nil)
			if err != nil {
				return err
			}
			if err := parent.ReloadExecutionResults(vm.db); err != nil {
				return err
			}
			chain = append(chain, parent)
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
