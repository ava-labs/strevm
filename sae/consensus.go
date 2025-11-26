// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/libevm/common"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
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
	_ = (*saexec.Executor)(nil)
	return errUnimplemented
}

// LastAccepted returns the ID of the last block received by [VM.AcceptBlock].
func (vm *VM) LastAccepted(context.Context) (ids.ID, error) {
	return ids.Empty, errUnimplemented
}

// RejectBlock is a no-op in SAE because execution only occurs after acceptance.
func (vm *VM) RejectBlock(ctx context.Context, b *blocks.Block) error {
	vm.blocks.Delete(b.Hash())
	return nil
}
