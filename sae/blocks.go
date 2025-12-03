// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"

	"github.com/ava-labs/strevm/blocks"
)

// ParseBlock parses the buffer as [rlp] encoding of a [types.Block].
func (vm *VM) ParseBlock(context.Context, []byte) (*blocks.Block, error) {
	ethB := new(types.Block)
	_ = rlp.DecodeBytes(nil, ethB)
	return nil, errUnimplemented
}

// BuildBlock builds a new block, using the last block passed to
// [VM.SetPreference] as the parent and the given block context.
func (vm *VM) BuildBlock(context.Context, *block.Context) (*blocks.Block, error) {
	return nil, errUnimplemented
}

// VerifyBlock validates the block with the given block context.
func (vm *VM) VerifyBlock(context.Context, *block.Context, *blocks.Block) error {
	return errUnimplemented
}

// GetBlock returns the block with the given ID, or [database.ErrNotFound].
func (vm *VM) GetBlock(context.Context, ids.ID) (*blocks.Block, error) {
	_ = database.ErrNotFound
	return nil, errUnimplemented
}

// GetBlockIDAtHeight returns the accepted block at the given height, or
// [database.ErrNotFound].
func (vm *VM) GetBlockIDAtHeight(context.Context, uint64) (ids.ID, error) {
	return ids.Empty, errUnimplemented
}
