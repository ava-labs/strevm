// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package worstcase provides the worst-case balance and nonce tracking needed
// to safely include transactions that are guaranteed to be valid during
// execution.
package worstcase

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	saeparams "github.com/ava-labs/strevm/params"
)

// State tracks the worst-case gas price and account state as operations are
// executed.
//
// Usage of the [State] should follow the pattern:
//  1. [State.StartBlock] for each block to be included.
//  2. [State.GasLimit] and [State.BaseFee] to query the block's parameters.
//  3. [State.ApplyTx] or [State.Apply] for each transaction or operation to
//     include in the block.
//  4. [State.FinishBlock] to finalize the block's gas time.
//  5. Repeat from step 1 for the next block.
type State struct {
	hooks  hook.Points
	config *params.ChainConfig

	db    *state.StateDB
	clock *gastime.Time

	qSize, blockSize, maxBlockSize gas.Gas

	baseFee *uint256.Int
	curr    *types.Header
	rules   params.Rules
	signer  types.Signer
}

// NewState constructs a new worst-case state.
//
// The [state.StateDB] MUST be opened at the state immediately following the
// last-executed block upon which the worst-case state is built. Similarly, the
// [gastime.Time] MUST be a clone of the gas clock at the same point. The
// StateDB will only be used as a scratchpad for tracking accounts, and will NOT
// be committed.
func NewState(
	hooks hook.Points,
	config *params.ChainConfig,
	db *state.StateDB,
	fromExecTime *gastime.Time,
) *State {
	return &State{
		hooks:  hooks,
		config: config,
		db:     db,
		clock:  fromExecTime,
	}
}

const rateToMaxBlockSize = saeparams.Tau * saeparams.Lambda

var (
	errNonConsecutiveBlocks = errors.New("non-consecutive block numbers")
	errQueueFull            = errors.New("queue full")
)

// StartBlock updates the worst-case state to the beginning of the provided
// block.
//
// It is not necessary for [types.Header.GasLimit] or [types.Header.BaseFee] to
// be set.
//
// If the queue is too full to accept another block, an error is returned.
func (s *State) StartBlock(hdr *types.Header) error {
	if c := s.curr; c != nil {
		if num, next := c.Number.Uint64(), hdr.Number.Uint64(); next != num+1 {
			return fmt.Errorf("%w: %d then %d", errNonConsecutiveBlocks, num, next)
		}
	}

	s.clock.BeforeBlock(s.hooks, hdr)
	s.blockSize = 0

	const (
		maxQSizeMultiplier = 2
		// In order to avoid overflow when calculating the queue size, we cap
		// the maximum gas rate to a safe value.
		//
		// This follows from:
		//   maxBlockSize = maxRate * Tau * Lambda
		//   maxQSizeInStart = maxQSizeMultiplier * maxBlockSize
		//   maxQSizeInFinish = maxQSizeInStart + maxBlockSize
		maxRate gas.Gas = math.MaxUint64 / rateToMaxBlockSize / (maxQSizeMultiplier + 1)
	)
	r := min(s.clock.Rate(), maxRate)
	s.maxBlockSize = r * rateToMaxBlockSize
	if maxQSize := maxQSizeMultiplier * s.maxBlockSize; s.qSize > maxQSize {
		return fmt.Errorf("%w: current size %d exceeds maximum size %d", errQueueFull, s.qSize, maxQSize)
	}

	s.baseFee = s.clock.BaseFee()

	s.curr = types.CopyHeader(hdr)
	s.curr.GasLimit = uint64(s.maxBlockSize)
	s.curr.BaseFee = s.baseFee.ToBig()

	// For both rules and signer, we MUST use the block's timestamp, not the
	// execution clock's, otherwise we might enable an upgrade too early.
	s.rules = s.config.Rules(hdr.Number, true, hdr.Time)
	s.signer = types.MakeSigner(s.config, hdr.Number, hdr.Time)
	return nil
}

// GasLimit returns the available gas limit for the current block.
func (s *State) GasLimit() uint64 {
	return uint64(s.maxBlockSize)
}

// BaseFee returns the worst-case base fee for the current block.
func (s *State) BaseFee() *uint256.Int {
	return s.baseFee
}

type (
	// AccountDebit includes an amount that an account should have debited,
	// along with the nonce used to debit the account.
	AccountDebit = hook.AccountDebit
	// Op is an operation that can be applied to a [State].
	Op = hook.Op
)

var (
	errGasFeeCapOverflow = errors.New("GasFeeCap() overflows uint256")
	errCostOverflow      = errors.New("Cost() overflows uint256")
)

// ApplyTx validates the transaction both intrinsically and in the context of
// worst-case gas assumptions of all previous operations. This provides an upper
// bound on the total cost of the transaction such that a nil error returned by
// ApplyTx guarantees that the sender of the transaction will have sufficient
// balance to cover its costs if consensus accepts the same operation set
// (and order) as was applied.
//
// If the transaction can not be applied, an error is returned and the state is
// not modified.
//
// TODO: Consider refactoring into a standalone function to convert transactions
// into Ops, rather than Applying internally.
func (s *State) ApplyTx(tx *types.Transaction) error {
	opts := &txpool.ValidationOptions{
		Config: s.config,
		Accept: 0 |
			1<<types.LegacyTxType |
			1<<types.AccessListTxType |
			1<<types.DynamicFeeTxType,
		MaxSize: math.MaxUint, // TODO(arr4n)
		MinTip:  big.NewInt(0),
	}
	if err := txpool.ValidateTransaction(tx, s.curr, s.signer, opts); err != nil {
		return fmt.Errorf("validating transaction: %w", err)
	}

	from, err := types.Sender(s.signer, tx)
	if err != nil {
		return fmt.Errorf("determining sender: %w", err)
	}

	var gasPrice uint256.Int
	if overflow := gasPrice.SetFromBig(tx.GasFeeCap()); overflow {
		// This should be unreachable due to the txpool validation.
		return errGasFeeCapOverflow
	}
	var amount uint256.Int
	if overflow := amount.SetFromBig(tx.Cost()); overflow {
		return errCostOverflow
	}
	return s.Apply(Op{
		Gas:      gas.Gas(tx.Gas()),
		GasPrice: gasPrice,
		From: map[common.Address]hook.AccountDebit{
			from: {
				Nonce:  tx.Nonce(),
				Amount: amount,
			},
		},
		// To is not populated here because this transaction may revert.
	})
}

// ErrBlockTooFull is returned by [State.ApplyTx] and [State.Apply] if inclusion
// would cause the block to exceed the gas limit.
var ErrBlockTooFull = errors.New("block too full")

// Apply attempts to apply the operation to this state.
//
// If the operation can not be applied, an error is returned and the state is
// not modified.
//
// Operations are invalid if:
//
//   - The operation consumes more gas than the block has available.
//   - The operation specifies too low of a gas price.
//   - The operation is from an account with an incorrect or invalid nonce.
//   - The operation is from an account with an insufficient balance.
func (s *State) Apply(o Op) error {
	// ----- Gas -----
	if o.Gas > s.maxBlockSize-s.blockSize {
		return ErrBlockTooFull
	}

	// ----- GasPrice -----
	if o.GasPrice.Cmp(s.baseFee) < 0 {
		return core.ErrFeeCapTooLow
	}

	// ----- From -----
	for from, ad := range o.From {
		switch nonce, next := ad.Nonce, s.db.GetNonce(from); {
		case nonce < next:
			return fmt.Errorf("%w: %d < %d", core.ErrNonceTooLow, nonce, next)
		case nonce > next:
			return fmt.Errorf("%w: %d > %d", core.ErrNonceTooHigh, nonce, next)
		case next == math.MaxUint64:
			return core.ErrNonceMax
		}

		if bal := s.db.GetBalance(from); ad.Amount.Cmp(bal) > 0 {
			return core.ErrInsufficientFunds
		}
	}

	// ----- Inclusion -----
	s.blockSize += o.Gas

	for from, ad := range o.From {
		s.db.SetNonce(from, ad.Nonce+1)
		s.db.SubBalance(from, &ad.Amount)
	}

	for to, amount := range o.To {
		s.db.AddBalance(to, &amount)
	}
	return nil
}

// FinishBlock advances the [gastime.Time] in preparation for the next block.
func (s *State) FinishBlock() error {
	if err := s.clock.AfterBlock(s.blockSize, s.hooks, s.curr); err != nil {
		return fmt.Errorf("finishing block gas time update: %w", err)
	}
	s.qSize += s.blockSize
	return nil
}
