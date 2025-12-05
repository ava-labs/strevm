// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

// SetPreference updates the VM's currently [preferred block] with the given block context,
// which MAY be nil.
//
// [preferred block]: https://github.com/ava-labs/avalanchego/tree/master/vms#set-preference
func (vm *VM) SetPreference(context.Context, ids.ID, *block.Context) error {
	return errUnimplemented
}

// AcceptBlock marks the block as [accepted], resulting in:
//   - All blocks settled by this block having their [blocks.Block.MarkSettled]
//     method called; and
//   - The block being propagated to [saexec.Executor.Enqueue].
//
// [accepted]: https://github.com/ava-labs/avalanchego/tree/master/vms#block-statuses
func (vm *VM) AcceptBlock(context.Context, *blocks.Block) error {
	_ = (*saexec.Executor)(nil)
	return errUnimplemented
}

// LastAccepted returns the ID of the last block received by [VM.AcceptBlock].
func (vm *VM) LastAccepted(context.Context) (ids.ID, error) {
	return ids.Empty, errUnimplemented
}

// RejectBlock is a no-op in SAE because execution only occurs after acceptance.
func (vm *VM) RejectBlock(context.Context, *blocks.Block) error {
	return nil
}
