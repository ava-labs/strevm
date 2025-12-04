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
	BeforeBlock(params.Rules, *state.StateDB, *types.Block) error
	AfterBlock(*state.StateDB, *types.Block, types.Receipts)
}

// BeforeBlock is intended to be called before processing a block, with the gas
// target sourced from [Points].
func BeforeBlock(pts Points, rules params.Rules, sdb *state.StateDB, b *blocks.Block, clock *gastime.Time) error {
	clock.FastForwardTo(
		b.BuildTime(),
		pts.SubSecondBlockTime(b.EthBlock()),
	)
	target := pts.GasTarget(b.ParentBlock().EthBlock())
	if err := clock.SetTarget(target); err != nil {
		return fmt.Errorf("%T.SetTarget() before block: %w", clock, err)
	}
	return pts.BeforeBlock(rules, sdb, b.EthBlock())
}

// AfterBlock is intended to be called after processing a block, with the gas
// sourced from [types.Block.GasUsed] or equivalent.
func AfterBlock(pts Points, sdb *state.StateDB, b *types.Block, clock *gastime.Time, used gas.Gas, rs types.Receipts) {
	clock.Tick(used)
	pts.AfterBlock(sdb, b, rs)
}

// MinimumGasConsumption MUST be used as the implementation for the respective
// method on [params.RulesHooks]. The concrete type implementing the hooks MUST
// propagate incoming and return arguments unchanged.
func MinimumGasConsumption(txLimit uint64) uint64 {
	_ = (params.RulesHooks)(nil) // keep the import to allow [] doc links
	return intmath.CeilDiv(txLimit, saeparams.Lambda)
}
