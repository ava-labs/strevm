// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
)

// SetPreference updates the VM's currently [preferred block].
//
// [preferred block]: https://github.com/ava-labs/avalanchego/tree/master/vms#set-preference
func (vm *VM) SetPreference(ctx context.Context, id ids.ID) error {
	b, ok := vm.blocks.Load(common.Hash(id))
	if !ok {
		return database.ErrNotFound
	}
	vm.preference.Store(b)
	return nil
}

// AcceptBlock marks the block as [accepted], resulting in:
//   - All blocks settled by this block having their [blocks.Block.MarkSettled]
//     method called; and
//   - The block being propagated to [saexec.Executor.Enqueue].
//
// [accepted]: https://github.com/ava-labs/avalanchego/tree/master/vms#block-statuses
func (vm *VM) AcceptBlock(ctx context.Context, b *blocks.Block) error {
	settles := b.Settles()
	{
		batch := vm.db.NewBatch()
		rawdb.WriteBlock(batch, b.EthBlock())
		rawdb.WriteCanonicalHash(batch, b.Hash(), b.NumberU64())
		rawdb.WriteTxLookupEntriesByBlock(batch, b.EthBlock()) // i.e. canonical tx inclusion
		if s := settles; len(s) > 0 {
			rawdb.WriteFinalizedBlockHash(batch, s[len(s)-1].Hash())
		}
		if err := batch.Write(); err != nil {
			return err
		}
	}
	for _, s := range settles {
		if err := s.MarkSettled(); err != nil {
			return err
		}
	}

	// The documentation of invariants and ordering guarantees explicitly states
	// that these can happen in any order because they involve different blocks
	// entering different states.
	vm.last.settled.Store(b.LastSettled())
	vm.last.accepted.Store(b)

	// This MUST NOT happen before the database and [VM.last] are updated to
	// reflect that the block has been accepted.
	if err := vm.exec.Enqueue(ctx, b); err != nil {
		return err
	}
	// When the chain is bootstrapping, avalanchego expects to be able to call
	// `Verify` and `Accept` in a loop over blocks. Reporting an error during
	// either `Verify` or `Accept` is considered FATAL during this process.
	// Therefore, we must ensure that avalanchego does not get too far ahead of
	// the execution thread and FATAL during block verification.
	if vm.snowState.Get() == snow.Bootstrapping {
		if err := b.WaitUntilExecuted(ctx); err != nil {
			return fmt.Errorf("waiting for block %d to execute: %v", b.Height(), err)
		}
	}

	vm.log().Debug(
		"Accepted block",
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)

	// Same rationale as the invariant described in [blocks.Block]. Praised be
	// the GC!
	keep := b.LastSettled().Hash()
	for _, s := range settles {
		if s.Hash() == keep {
			// Removed (below) when this block's child is accepted.
			continue
		}
		vm.blocks.Delete(s.Hash())
	}
	if s := b.ParentBlock().LastSettled(); s != nil && s.Hash() != keep {
		vm.blocks.Delete(s.Hash())
	}
	return nil
}

// LastAccepted returns the ID of the last block received by [VM.AcceptBlock].
func (vm *VM) LastAccepted(context.Context) (ids.ID, error) {
	return vm.last.accepted.Load().ID(), nil
}

// RejectBlock is a no-op in SAE because execution only occurs after acceptance.
func (vm *VM) RejectBlock(ctx context.Context, b *blocks.Block) error {
	vm.blocks.Delete(b.Hash())
	return nil
}
