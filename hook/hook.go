// Package hook defines points in a block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"context"
	"iter"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/gastime"
)

// Points define user-injected hook points.
type Points interface {
	GasTarget(parent *types.Block) gas.Gas
	// ShouldVerifyBlockContext reports whether this block is only valid against
	// a subset of proposervm block contexts. If there are contexts where this
	// block could be invalid, this function must return true.
	//
	// This function must be deterministic for a given block.
	ShouldVerifyBlockContext(ctx context.Context, block *types.Block) (bool, error)
	// VerifyBlockContext verifies that the block is valid within the provided
	// block context. This is not expected to fully verify the block, only that
	// the block is not invalid with the provided context.
	VerifyBlockContext(ctx context.Context, blockContext *block.Context, block *types.Block) error
	// VerifyBlockAncestors verifies that the block has a valid chain of
	// ancestors. This is not expected to fully verify the block, only that the
	// block's ancestors are compatible. The ancestor iterator iterates from the
	// parent of block up to but not including the most recently settled block.
	VerifyBlockAncestors(ctx context.Context, block *types.Block, ancestors iter.Seq[*types.Block]) error
}

// BeforeBlock is intended to be called before processing a block, with the gas
// target sourced from [Points].
func BeforeBlock(clock *gastime.Time, block *types.Header, target gas.Gas) {
	clock.FastForwardTo(block.Time)
	clock.SetTarget(target)
}

// AfterBlock is intended to be called after processing a block, with the gas
// sourced from [types.Block.GasUsed] or equivalent.
func AfterBlock(clock *gastime.Time, used gas.Gas) {
	clock.Tick(used)
}
