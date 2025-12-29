// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hook defines points in an SAE block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"context"
	"math"

	"github.com/ava-labs/avalanchego/ids"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
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
//
// Directly using this interface as a [BlockBuilder] is indicative of this node
// locally building a block. Calling [Points.BlockRebuilderFrom] with an
// existing block is indicative of this node reconstructing a block built
// elsewhere during verification.
type Points interface {
	// BlockEvents is called every time [snowcommon.VM.WaitForEvent] is invoked.
	// If it returns a non-nil error, the error will be propagated to the caller
	// of [snowcommon.VM.WaitForEvent].
	BlockEvents(context.Context) error
	// WaitForEvent is called to wait for the next message for the consensus
	// engine.
	WaitForEvent(context.Context) (snowcommon.Message, error)

	BlockBuilder
	// BlockRebuilderFrom returns a [BlockBuilder] that will attempt to
	// reconstruct the provided block. If the provided block is valid for
	// inclusion, then the returned builder MUST be able to reconstruct an
	// identical block.
	BlockRebuilderFrom(block *types.Block) BlockBuilder

	// GasTargetAfter returns the gas target that should go into effect
	// immediately after the provided block.
	GasTargetAfter(*types.Header) gas.Gas
	// SubSecondBlockTime returns the sub-second portion of the block time based
	// on the provided gas rate.
	//
	// For example, if the block timestamp is 10.75 seconds and the gas rate is
	// 100 gas/second, then this method should return 75 gas.
	SubSecondBlockTime(gasRate gas.Gas, h *types.Header) gas.Gas
	// EndOfBlockOps returns operations outside of the normal EVM state changes
	// to perform while executing the block, after regular EVM transactions.
	// These operations will be performed during both worst-case and actual
	// execution.
	EndOfBlockOps(*types.Block) []Op
	// BeforeExecutingBlock is called immediately prior to executing the block.
	BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error
	// AfterExecutingBlock is called immediately after executing the block.
	AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts)
}

// BlockBuilder constructs a block given its components.
type BlockBuilder interface {
	// BuildHeader constructs a header from the parent header.
	//
	// The returned header MUST have [types.Header.ParentHash],
	// [types.Header.Number] and [types.Header.Time] set appropriately.
	// [types.Header.Root], [types.Header.GasLimit], [types.Header.BaseFee], and
	// [types.Header.GasUsed] will be ignored and overwritten. Any other fields
	// MAY be set as desired.
	//
	// SAE always uses this method instead of directly constructing a header, to
	// ensure any libevm header extras are properly populated.
	BuildHeader(parent *types.Header) *types.Header
	// BuildBlock constructs a block with the given components.
	//
	// SAE always uses this method instead of [types.NewBlock], to ensure any
	// libevm block extras are properly populated.
	BuildBlock(
		header *types.Header,
		txs []*types.Transaction,
		receipts []*types.Receipt,
	) *types.Block
}

// AccountDebit includes an amount that an account should have debited,
// along with the nonce used to aut debit the account.
type AccountDebit struct {
	Nonce  uint64
	Amount uint256.Int
}

// Op is an operation that can be applied to state during the execution of a
// block.
type Op struct {
	// ID of this operation. It is used for logging and debugging purposes.
	ID ids.ID
	// Gas consumed by this operation.
	Gas gas.Gas
	// GasFeeCap is the maximum gas price this operation is willing to pay.
	GasFeeCap uint256.Int
	// Burn specifies the amount to decrease account balances by and the nonce
	// used to authorize the debit.
	Burn map[common.Address]AccountDebit
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
	for from, acc := range o.Burn {
		if b := stateDB.GetBalance(from); b.Lt(&acc.Amount) {
			return core.ErrInsufficientFunds
		}
	}
	for from, acc := range o.Burn {
		// We use the state as the source of truth for the current nonce rather
		// than the value provided by the hook. This prevents any situations,
		// such as with delegated accounts, where nonces might not be
		// incremented properly.
		//
		// If overflow would have occurred here, the nonce must have already
		// been increased by a delegated account's execution, so we are already
		// protected against replay attacks.
		if nonce := stateDB.GetNonce(from); nonce < math.MaxUint64 {
			stateDB.SetNonce(from, nonce+1)
		}
		stateDB.SubBalance(from, &acc.Amount)
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
