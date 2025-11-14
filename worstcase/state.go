// Package worstcase is a pessimist, always seeing the glass as half empty. But
// where others see full glasses and opportunities, package worstcase sees DoS
// vulnerabilities.
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
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/holiman/uint256"
)

// A State assumes that every transaction will consume its stated
// gas limit, tracking worst-case gas costs under this assumption.
type State struct {
	db *state.StateDB

	curr   *types.Header
	config *params.ChainConfig
	rules  params.Rules
	signer types.Signer

	clock *gastime.Time

	maxQSeconds, maxBlockSeconds                 uint64
	qLength, maxQLength, blockSize, maxBlockSize gas.Gas
}

// NewTxIncluder constructs a new includer.
//
// The [state.StateDB] MUST be opened at the state immediately following the
// last-executed block upon which the includer is building. Similarly, the
// [gastime.Time] MUST be a clone of the gas clock at the same point. The
// StateDB will only be used as a scratchpad for tracking accounts, and will NOT
// be committed.
//
// [State.StartBlock] MUST be called before the first call to
// [State.Include].
func NewTxIncluder(
	db *state.StateDB,
	config *params.ChainConfig,
	fromExecTime *gastime.Time,
	maxQueueSeconds, maxBlockSeconds uint64,
) *State {
	s := &State{
		db:              db,
		config:          config,
		clock:           fromExecTime,
		maxQSeconds:     maxQueueSeconds,
		maxBlockSeconds: maxBlockSeconds,
	}
	s.setMaxSizes()
	return s
}

func (s *State) setMaxSizes() {
	s.maxQLength = s.clock.Rate() * gas.Gas(s.maxQSeconds)
	s.maxBlockSize = s.clock.Rate() * gas.Gas(s.maxBlockSeconds)
}

var errNonConsecutiveBlocks = errors.New("non-consecutive block numbers")

// StartBlock calls [State.FinishBlock] and then fast-forwards the
// includer's [gastime.Time] to the new block's timestamp before updating the
// gas target. Only the block number and timestamp are required to be set in the
// header.
func (s *State) StartBlock(hdr *types.Header, target gas.Gas) error {
	if c := s.curr; c != nil {
		if num, next := c.Number.Uint64(), hdr.Number.Uint64(); next != num+1 {
			return fmt.Errorf("%w: %d then %d", errNonConsecutiveBlocks, num, next)
		}
	}

	s.FinishBlock()
	hook.BeforeBlock(s.clock, hdr, target)
	s.setMaxSizes()
	s.curr = types.CopyHeader(hdr)
	s.curr.GasLimit = uint64(min(s.maxQLength, s.maxBlockSize))

	// For both rules and signer, we MUST use the block's timestamp, not the
	// execution clock's, otherwise we might enable an upgrade too early.
	s.rules = s.config.Rules(hdr.Number, true, hdr.Time)
	s.signer = types.MakeSigner(s.config, hdr.Number, hdr.Time)
	return nil
}

// FinishBlock advances the includer's [gastime.Time] to account for all
// included transactions since the last call to FinishBlock. In the absence of
// intervening calls to [State.Include], calls to FinishBlock are
// idempotent.
//
// There is no need to call FinishBlock before a call to
// [State.StartBlock].
func (s *State) FinishBlock() {
	hook.AfterBlock(s.clock, s.blockSize)
	s.blockSize = 0
}

// ErrQueueTooFull and ErrBlockTooFull are returned by
// [State.Include] if inclusion of the transaction would have
// caused the queue or block, respectively, to exceed their maximum allowed gas
// length.
var (
	ErrQueueTooFull = errors.New("queue too full")
	ErrBlockTooFull = errors.New("block too full")
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
		return err
	}

	from, err := types.Sender(s.signer, tx)
	if err != nil {
		return fmt.Errorf("determining sender: %w", err)
	}

	return s.Apply(hook.Op{
		Gas:      gas.Gas(tx.Gas()),
		GasPrice: *uint256.MustFromBig(tx.GasFeeCap()),
		From: map[common.Address]hook.Account{
			from: {
				Nonce:  tx.Nonce(),
				Amount: *uint256.MustFromBig(tx.Cost()),
			},
		},
		// To is not populated here because this transaction may revert.
	})
}

// Apply attempts to apply the operation to this state.
//
// If the operation can not be applied, an error is returned and the state is
// not modified.
//
// Operations are invalid if:
// - The operation consumes more gas than currently allowed.
// - The operation specifies too low of a gas price.
// - The operation specifies a From account with an incorrect or invalid nonce.
// - The operation specifies a From account with an insufficient balance.
func (s *State) Apply(o hook.Op) error {
	// ----- Gas -----
	switch {
	case o.Gas > s.maxQLength-s.qLength:
		return ErrQueueTooFull
	case o.Gas > s.maxBlockSize-s.blockSize:
		return ErrBlockTooFull
	}

	// ----- GasPrice -----
	if min := s.clock.BaseFee(); o.GasPrice.Cmp(min) < 0 {
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

		if bal := s.db.GetBalance(from); bal.Cmp(&ad.Amount) < 0 {
			return core.ErrInsufficientFunds
		}
	}

	// ----- Inclusion -----
	s.qLength += o.Gas
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
