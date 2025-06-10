// Package worstcase is a pessimist, always seeing the glass as half empty. But
// where others see full glasses and opportunities, package worstcase sees DoS
// vulnerabilities.
package worstcase

import (
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/holiman/uint256"
)

// A TransactionIncluder assumes that every transaction will consume its stated
// gas limit, tracking worst-case gas costs under this assumption.
type TransactionIncluder struct {
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
// [TransactionIncluder.StartBlock] MUST be called before the first call to
// [TransactionIncluder.Include].
func NewTxIncluder(
	db *state.StateDB,
	config *params.ChainConfig,
	fromExecTime *gastime.Time,
	maxQueueSeconds, maxBlockSeconds uint64,
) *TransactionIncluder {
	inc := &TransactionIncluder{
		db:              db,
		config:          config,
		clock:           fromExecTime,
		maxQSeconds:     maxQueueSeconds,
		maxBlockSeconds: maxBlockSeconds,
	}
	inc.setMaxSizes()
	return inc
}

func (inc *TransactionIncluder) setMaxSizes() {
	inc.maxQLength = inc.clock.Rate() * gas.Gas(inc.maxQSeconds)
	inc.maxBlockSize = inc.clock.Rate() * gas.Gas(inc.maxBlockSeconds)
}

var errNonConsecutiveBlocks = errors.New("non-consecutive block numbers")

// StartBlock calls [TransactionIncluder.FinishBlock] and then fast-forwards the
// includer's [gastime.Time] to the new block's timestamp before updating the
// gas target. Only the block number and timestamp are required to be set in the
// header.
func (inc *TransactionIncluder) StartBlock(hdr *types.Header, target gas.Gas) error {
	if c := inc.curr; c != nil {
		if num, next := c.Number.Uint64(), hdr.Number.Uint64(); next != num+1 {
			return fmt.Errorf("%w: %d then %d", errNonConsecutiveBlocks, num, next)
		}
	}

	inc.FinishBlock()
	hook.BeforeBlock(inc.clock, hdr, target)
	inc.setMaxSizes()
	inc.curr = types.CopyHeader(hdr)

	// For both rules and signer, we MUST use the block's timestamp, not the
	// execution clock's, otherwise we might enable an upgrade too early.
	inc.rules = inc.config.Rules(hdr.Number, true, hdr.Time)
	inc.signer = types.MakeSigner(inc.config, hdr.Number, hdr.Time)

	return nil
}

// FinishBlock advances the includer's [gastime.Time] to account for all
// included transactions since the last call to FinishBlock. In the absence of
// intervening calls to [TransactionIncluder.Include], calls to FinishBlock are
// idempotent.
//
// There is no need to call FinishBlock before a call to
// [TransactionIncluder.StartBlock].
func (inc *TransactionIncluder) FinishBlock() {
	hook.AfterBlock(inc.clock, inc.blockSize)
	inc.blockSize = 0
}

// ErrQueueTooFull and ErrBlockTooFull are returned by
// [TransactionIncluder.Include] if inclusion of the transaction would have
// caused the queue or block, respectively, to exceed their maximum allowed gas
// length.
var (
	ErrQueueTooFull = errors.New("queue too full")
	ErrBlockTooFull = errors.New("block too full")
)

// Include validates the transaction both intrinsically and in the context of
// worst-case gas assumptions of all previous calls to Include. This provides an
// upper bound on the total cost of the transaction such that a nil error
// returned by Include guarantees that the sender of the transaction will have
// sufficient balance to cover its costs if consensus accepts the same
// transaction set (and order) as was passed to Include.
//
// The TransactionIncluder's internal state is updated to reflect inclusion of
// the transaction i.f.f. a nil error is returned by Include.
func (inc *TransactionIncluder) Include(tx *types.Transaction) error {
	switch g := gas.Gas(tx.Gas()); {
	case g > inc.maxQLength-inc.qLength:
		return ErrQueueTooFull
	case g > inc.maxBlockSize-inc.blockSize:
		return ErrBlockTooFull
	}
	if err := checkStateless(tx, inc.rules); err != nil {
		return err
	}

	from, err := types.Sender(inc.signer, tx)
	if err != nil {
		return fmt.Errorf("determining sender: %w", err)
	}

	// ----- Nonce -----
	switch nonce, next := tx.Nonce(), inc.db.GetNonce(from); {
	case nonce < next:
		return fmt.Errorf("%w: %d < %d", core.ErrNonceTooLow, nonce, next)
	case nonce > next:
		return fmt.Errorf("%w: %d > %d", core.ErrNonceTooHigh, nonce, next)
	case next+1 < next:
		return core.ErrNonceMax
	}

	// ----- Balance covers worst-case gas cost + tx value -----
	price := inc.clock.BaseFee()
	if cap, min := tx.GasFeeCap(), price.ToBig(); cap.Cmp(min) < 0 {
		return core.ErrFeeCapTooLow
	}
	gasCost := new(uint256.Int).Mul(
		price,
		uint256.NewInt(tx.Gas()),
	)
	txCost := new(uint256.Int).Add(
		gasCost,
		uint256.MustFromBig(tx.Value()),
	)
	if bal := inc.db.GetBalance(from); bal.Cmp(txCost) < 0 {
		return core.ErrInsufficientFunds
	}

	// ----- Inclusion -----
	g := gas.Gas(tx.Gas())
	inc.qLength += g
	inc.blockSize += g

	inc.db.SetNonce(from, inc.db.GetNonce(from)+1)
	inc.db.SubBalance(from, txCost)

	return nil
}

func checkStateless(tx *types.Transaction, rules params.Rules) error {
	contractCreation := tx.To() == nil

	// ----- Init-code length -----
	if contractCreation && len(tx.Data()) > params.MaxInitCodeSize {
		return core.ErrMaxInitCodeSizeExceeded
	}

	// ----- Intrinsic gas -----
	intrinsic, err := core.IntrinsicGas(
		tx.Data(), tx.AccessList(),
		contractCreation, rules.IsHomestead,
		rules.IsIstanbul, // EIP-2028
		rules.IsShanghai, // EIP-3869
	)
	if err != nil {
		return fmt.Errorf("calculating intrinsic gas requirement: %w", err)
	}
	if tx.Gas() < intrinsic {
		return fmt.Errorf("%w: %d < %d", core.ErrIntrinsicGas, tx.Gas(), intrinsic)
	}

	return nil
}
