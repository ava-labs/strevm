// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hook defines points in an SAE block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/intmath"
	saeparams "github.com/ava-labs/strevm/params"
)

// Points define user-injected hook points.
type Points interface {
	GasTarget(parent *types.Block) gas.Gas
	BeforeBlock(*types.Block, params.Rules, *state.StateDB) error
	AfterBlock(*types.Block)
}

// BeforeBlock is intended to be called before processing a block, with the gas
// target sourced from [Points].
func BeforeBlock(pts Points, b *types.Block, rules params.Rules, sdb *state.StateDB, clock *gastime.Time, target gas.Gas) error {
	clock.FastForwardTo(b.Time())
	if err := clock.SetTarget(target); err != nil {
		return err
	}
	return pts.BeforeBlock(b, rules, sdb)
}

// AfterBlock is intended to be called after processing a block, with the gas
// sourced from [types.Block.GasUsed] or equivalent.
func AfterBlock(pts Points, b *types.Block, clock *gastime.Time, used gas.Gas) {
	clock.Tick(used)
	pts.AfterBlock(b)
}

// MinimumGasConsumption MUST be used as the implementation for the respective
// method on [params.RulesHooks]. The concrete type implementing the hooks MUST
// propagate incoming and return arguments unchanged.
func MinimumGasConsumption(txLimit uint64) uint64 {
	_ = (params.RulesHooks)(nil) // keep the import to allow [] doc links
	return intmath.CeilDiv(txLimit, saeparams.Lambda)
}
