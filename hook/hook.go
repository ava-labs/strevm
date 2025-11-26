// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hook defines points in an SAE block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"fmt"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/intmath"
	saeparams "github.com/ava-labs/strevm/params"
)

// Points define user-injected hook points.
type Points interface {
	GasTarget(parent *types.Block) gas.Gas
	SubSecondBlockTime(*types.Block) gas.Gas
	BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error
	AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts)
}

// BeforeBuildingBlock MUST be called before building a block.

// BeforeExecutingBlock MUST be called before executing a block.
func BeforeExecutingBlock(pts Points, rules params.Rules, sdb *state.StateDB, b *blocks.Block, clock *gastime.Time) error {
	frac := pts.SubSecondBlockTime(b.EthBlock())
	if err := beforeBlock(pts, b, b.BuildTime(), frac, clock); err != nil {
		return err
	}
	return pts.BeforeExecutingBlock(rules, sdb, b.EthBlock())
}

func beforeBlock(pts Points, b *blocks.Block, timeStamp uint64, subSecond gas.Gas, clock *gastime.Time) error {
	clock.FastForwardTo(timeStamp, subSecond)
	if err := clock.SetTarget(b.GasTarget()); err != nil {
		return fmt.Errorf("%T.SetTarget() before block: %w", clock, err)
	}
	return nil
}

// AfterBuildingBlock MUST be called after building a block.
func AfterBuildingBlock(clock *gastime.Time, included types.Transactions) {
	var consumed gas.Gas
	for _, tx := range included {
		consumed += gas.Gas(tx.Gas())
	}
	afterBlock(clock, consumed)
}

// AfterExecutingBlock MUST be called after executing a block, with the consumed
// gas sourced from [types.Block.GasUsed] or equivalent.
func AfterExecutingBlock(pts Points, sdb *state.StateDB, b *types.Block, clock *gastime.Time, consumed gas.Gas, rs types.Receipts) {
	afterBlock(clock, consumed)
	pts.AfterExecutingBlock(sdb, b, rs)
}

func afterBlock(clock *gastime.Time, consumed gas.Gas) {
	clock.Tick(consumed)
}

// MinimumGasConsumption MUST be used as the implementation for the respective
// method on [params.RulesHooks]. The concrete type implementing the hooks MUST
// propagate incoming and return arguments unchanged.
func MinimumGasConsumption(txLimit uint64) uint64 {
	_ = (params.RulesHooks)(nil) // keep the import to allow [] doc links
	return intmath.CeilDiv(txLimit, saeparams.Lambda)
}
