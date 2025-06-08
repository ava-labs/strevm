// Package worstcase is a pessimist, always seeing the glass as half empty. But
// where others see full glasses and opportunities, package worstcase sees DoS
// vulnerabilities.
package worstcase

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/gastime"
	"github.com/holiman/uint256"
)

type TransactionIncluder struct {
	db *state.StateDB

	config *params.ChainConfig
	rules  params.Rules
	signer types.Signer

	clock *gastime.Time

	qLength, maxQLength, blockSize, maxBlockSize gas.Gas
}

func NewTxIncluder(
	db *state.StateDB,
	config *params.ChainConfig,
	fromHeight *big.Int,
	fromBlockTime uint64,
	fromExecTime *gastime.Time,
	maxQueueSeconds, maxBlockSeconds uint64,
) *TransactionIncluder {
	t := fromExecTime
	inc := &TransactionIncluder{
		db:           db,
		config:       config,
		clock:        t,
		maxQLength:   t.Rate() * gas.Gas(maxQueueSeconds),
		maxBlockSize: t.Rate() * gas.Gas(maxBlockSeconds),
	}
	inc.StartBlock(fromHeight, fromBlockTime)
	return inc
}

func (inc *TransactionIncluder) StartBlock(num *big.Int, timestamp uint64) {
	inc.clock.Tick(inc.blockSize)
	inc.blockSize = 0

	inc.clock.FastForwardTo(timestamp)

	// For both rules and signer, we MUST use the block's timestamp, not the
	// execution clock's, otherwise we might enable an upgrade too early.
	inc.rules = inc.config.Rules(num, true, timestamp)
	inc.signer = types.MakeSigner(inc.config, num, timestamp)
}

var (
	ErrQueueTooFull = errors.New("queue too full")
	ErrBlockTooFull = errors.New("block too full")
)

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
	price := uint256.NewInt(uint64(inc.clock.Price()))
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
