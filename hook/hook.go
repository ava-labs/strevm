// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hook defines points in an SAE block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"math"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/intmath"
	saeparams "github.com/ava-labs/strevm/params"
)

// Points define user-injected hook points.
type Points interface {
	// GasTargetAfter returns the gas target that should go into effect
	// immediately after the provided block.
	GasTargetAfter(*types.Header) gas.Gas
	// SubSecondBlockTime returns the sub-second portion of the block time based
	// on the provided gas rate.
	//
	// For example, if the block timestamp is 10.75 seconds and the gas rate is
	// 100 gas/second, then this method should return 75 gas.
	SubSecondBlockTime(gasRate gas.Gas, h *types.Header) gas.Gas
	// ExtraBlockOps returns operations outside of the normal EVM state changes
	// to perform while executing the block. These operations will be performed
	// during both worst-case and actual execution.
	ExtraBlockOps(*types.Block) []Op
	// BeforeExecutingBlock is called immediately prior to executing the block.
	BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error
	// AfterExecutingBlock is called immediately after executing the block.
	AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts)
}

// Op is an operation that can be applied to state during the execution of a
// block.
type Op struct {
	// ID of this operation. It is used for logging and debugging purposes.
	ID ids.ID
	// Gas consumed by this operation.
	Gas gas.Gas
	// Burn specifies the amount to decrease account balances by.
	Burn map[common.Address]uint256.Int
	// Mint specifies the amount to increase account balances by. These funds
	// are not necessarily tied to the funds consumed in the Burn field. The
	// sum of the Mint amounts may exceed the sum of the Burn amounts.
	Mint map[common.Address]uint256.Int
}

// ApplyTo applies the operation to the statedb.
//
// If an account has insufficient funds, [core.ErrInsufficientFunds] is returned
// and the statedb is unchanged.
func (o *Op) ApplyTo(stateDB *state.StateDB) error {
	for from, amount := range o.Burn {
		if b := stateDB.GetBalance(from); b.Lt(&amount) {
			return core.ErrInsufficientFunds
		}
	}
	for from, amount := range o.Burn {
		// If overflow would have occurred here, the nonce must have already
		// been increased by a delegated account's execution, so we are already
		// protected against replay attacks.
		if nonce := stateDB.GetNonce(from); nonce < math.MaxUint64 {
			stateDB.SetNonce(from, nonce+1)
		}
		stateDB.SubBalance(from, &amount)
	}
	for to, amount := range o.Mint {
		stateDB.AddBalance(to, &amount)
	}
	return nil
}

// MinimumGasConsumption MUST be used as the implementation for the respective
// method on [params.RulesHooks]. The concrete type implementing the hooks MUST
// propagate incoming and return arguments unchanged.
func MinimumGasConsumption(txLimit uint64) uint64 {
	_ = (params.RulesHooks)(nil) // keep the import to allow [] doc links
	return intmath.CeilDiv(txLimit, saeparams.Lambda)
}
