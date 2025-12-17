// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"

	"github.com/ava-labs/strevm/blocks"
)

// SetPreference updates the VM's currently [preferred block] with the given block context,
// which MAY be nil.
//
// [preferred block]: https://github.com/ava-labs/avalanchego/tree/master/vms#set-preference
func (vm *VM) SetPreference(ctx context.Context, id ids.ID, bCtx *block.Context) error {
	b, err := vm.GetBlock(ctx, id)
	if err != nil {
		return err
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
	// TODO(arr4n) implement settlement, updating of database canonical block,
	// head, etc.
	vm.lastAccepted.Store(b)
	return vm.exec.Enqueue(ctx, b)
}

// LastAccepted returns the ID of the last block received by [VM.AcceptBlock].
func (vm *VM) LastAccepted(context.Context) (ids.ID, error) {
	return vm.lastAccepted.Load().ID(), nil
}

// RejectBlock is a no-op in SAE because execution only occurs after acceptance.
func (vm *VM) RejectBlock(ctx context.Context, b *blocks.Block) error {
	vm.blocks.Delete(b.Hash())
	return nil
}
