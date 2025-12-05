// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hook defines points in an SAE block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/intmath"
	saeparams "github.com/ava-labs/strevm/params"
)

type Account struct {
	Nonce  uint64
	Amount uint256.Int
}

type Op struct {
	// Gas consumed by this operation
	Gas gas.Gas
	// GasPrice this operation is willing to spend
	GasPrice uint256.Int
	// From specifies the set of accounts and the authorization of funds to be
	// removed from the accounts.
	From map[common.Address]Account
	// To specifies the amount to increase account balances by. These funds are
	// not necessarily tied to the funds consumed in the From field. The sum of
	// the To amounts may even exceed the sum of the From amounts.
	To map[common.Address]uint256.Int
}

// Points define user-injected hook points.
type Points interface {
	// GasTarget returns the amount of gas per second that the chain should
	// target to consume after executing the provided block.
	GasTarget(*types.Header) gas.Gas
	// SubSecondBlockTime returns the sub-second portion of the block time based
	// on the provided gas rate.
	//
	// For example, if the block timestamp is 10.75 seconds and the gas rate is
	// 100 gas/second, then this method should return 75 gas.
	SubSecondBlockTime(gasRate gas.Gas, h *types.Header) gas.Gas
	// BeforeBlock is called immediately prior to executing the block.
	BeforeBlock(params.Rules, *state.StateDB, *types.Block) error
	// ExtraBlockOps returns operations outside of the normal EVM state changes
	// to perform while executing the block. These operations should be
	// performed after executing the normal ethereum transactions in the block.
	ExtraBlockOps(*types.Block) ([]Op, error)
	// AfterBlock is called immediately after executing the block.
	AfterBlock(*state.StateDB, *types.Block, types.Receipts)
}

// MinimumGasConsumption MUST be used as the implementation for the respective
// method on [params.RulesHooks]. The concrete type implementing the hooks MUST
// propagate incoming and return arguments unchanged.
func MinimumGasConsumption(txLimit uint64) uint64 {
	_ = (params.RulesHooks)(nil) // keep the import to allow [] doc links
	return intmath.CeilDiv(txLimit, saeparams.Lambda)
}
