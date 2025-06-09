package sae

import (
	"fmt"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

type validityChecker struct {
	db    *state.StateDB
	log   logging.Logger
	rules params.Rules

	gasClock    gasClock
	queueLength gas.Gas

	nonces   map[common.Address]uint64
	balances map[common.Address]*uint256.Int
}

type txValidity uint64

const (
	unspecifiedTxValidity txValidity = iota
	includeTx
	delayTx
	discardTx
	queueFull
)

func (vc *validityChecker) addTxToQueue(t txAndSender) (txValidity, error) {
	newQLength := vc.queueLength + gas.Gas(t.tx.Gas())
	if newQLength > vc.gasClock.maxQueueLength() {
		vc.log.Verbo(
			"Queue full",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Uint64("tx_gas_limit", t.tx.Gas()),
			zap.Uint64("curr_queue_length", uint64(vc.queueLength)),
		)
		return queueFull, nil
	}

	tx := t.tx
	from := t.from

	contractCreation := tx.To() == nil
	intrinsic, err := core.IntrinsicGas(
		tx.Data(), tx.AccessList(),
		contractCreation, vc.rules.IsHomestead,
		vc.rules.IsIstanbul, // EIP-2028
		vc.rules.IsShanghai, // EIP-3869
	)
	if err != nil {
		vc.log.Info(
			"Unable to determine intrinsic gas",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Error(err),
		)
		return discardTx, err
	}
	if tx.Gas() < intrinsic {
		vc.log.Verbo(
			"Insufficient intrinsic gas",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Uint64("tx", tx.Gas()),
			zap.Uint64("intrinsic", intrinsic),
		)
		return discardTx, nil
	}
	if contractCreation && len(tx.Data()) > params.MaxInitCodeSize {
		vc.log.Verbo(
			"Contract creation exceeds max init size",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Int("data_len", len(tx.Data())),
		)
		return discardTx, nil
	}

	switch nonce, next := tx.Nonce(), vc.nonce(from); {
	case nonce < next:
		vc.log.Verbo(
			"Nonce already used",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Uint64("tx", nonce),
			zap.Uint64("expect", next),
		)
		return discardTx, nil
	case nonce > next:
		vc.log.Verbo(
			"Future nonce",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Uint64("tx", nonce),
			zap.Uint64("next", next),
		)
		return delayTx, nil
	}

	price := vc.gasClock.params.baseFee()
	if tx.GasFeeCap().Cmp(price.ToBig()) < 0 {
		vc.log.Verbo(
			"Gas-fee cap below base fee",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Stringer("cap", tx.GasFeeCap()),
			zap.Stringer("base_fee", price),
		)
		return delayTx, nil
	}
	gasCost := new(uint256.Int).Mul(
		price,
		uint256.NewInt(tx.Gas()),
	)
	cost := gasCost.Add(gasCost, uint256.MustFromBig(tx.Value()))

	if !vc.trySpend(from, cost) {
		vc.log.Verbo(
			"Insufficient balance",
			zap.Stringer("tx_hash", t.tx.Hash()),
			zap.Stringer("balance", vc.balance(from)),
			zap.Stringer("cost", cost),
		)
		return delayTx, nil
	}
	vc.queueLength += gas.Gas(tx.Gas())
	vc.incrementNonce(from)
	return includeTx, nil
}

func (vc *validityChecker) nonce(addr common.Address) uint64 {
	n, ok := vc.nonces[addr]
	if !ok {
		n = vc.db.GetNonce(addr)
		vc.nonces[addr] = n
	}
	return n
}

func (vc *validityChecker) incrementNonce(addr common.Address) {
	vc.nonces[addr] = vc.nonce(addr) + 1
}

func (vc *validityChecker) balance(addr common.Address) *uint256.Int {
	b, ok := vc.balances[addr]
	if !ok {
		b = vc.db.GetBalance(addr)
		vc.balances[addr] = b
	}
	return b
}

func (vc *validityChecker) trySpend(addr common.Address, amt *uint256.Int) bool {
	bal := vc.balance(addr)
	if amt.Cmp(bal) == 1 {
		return false
	}
	bal.Sub(bal, amt) // updates the receiver so no need to set in the map again
	return true
}

func (v txValidity) String() string {
	switch v {
	case includeTx:
		return "include_tx"
	case delayTx:
		return "delay_tx"
	case discardTx:
		return "discard_tx"
	default:
		return fmt.Sprintf("%T(%d)", v, v)
	}
}
