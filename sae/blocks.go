// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/params"
)

// ParseBlock parses the buffer as [rlp] encoding of a [types.Block].
func (vm *VM) ParseBlock(ctx context.Context, buf []byte) (*blocks.Block, error) {
	ethB := new(types.Block)
	if err := rlp.DecodeBytes(buf, ethB); err != nil {
		return nil, fmt.Errorf("rlp.DecodeBytes(..., %T): %v", ethB, err)
	}
	return blocks.New(ethB, nil, nil, vm.log)
}

var errExecutionLagging = errors.New("execution lagging for settlement")

// BuildBlock builds a new block, using the last block passed to
// [VM.SetPreference] as the parent.
func (vm *VM) BuildBlock(context.Context) (*blocks.Block, error) {
	parent := vm.preference.Load()

	settleAt := uint64(vm.now().Add(-params.Tau).Unix()) //nolint:gosec // Guaranteed to be positive
	lastSettled, ok := blocks.LastToSettleAt(settleAt, parent)
	if !ok {
		return nil, errExecutionLagging
	}

	return blocks.New(nil, parent, lastSettled, vm.log)
}

// VerifyBlock validates the block.
func (vm *VM) VerifyBlock(context.Context, *blocks.Block) error {
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

var _ blocks.Source = (*VM)(nil).blockSource

func (vm *VM) blockSource(hash common.Hash, num uint64) (*blocks.Block, bool) {
	return nil, false
}
