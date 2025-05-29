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
	// TODO(arr4n) to fix the GMX issue, compare vc.queueLength (without the
	// addition) instead of `newQLength`.
	newQLength := vc.queueLength + gas.Gas(t.tx.Gas())
	if newQLength > stateRootDelaySeconds*maxGasPerSecond*lambda {
		vc.log.Verbo(
			"Queue full",
			zap.Uint64("length", uint64(vc.queueLength)),
			zap.Uint64("tx_gas_limit", t.tx.Gas()),
			zap.Stringer("tx_hash", t.tx.Hash()),
		)
		return queueFull, nil
	}

	tx := t.tx
	from := t.from

	switch nonce, next := tx.Nonce(), vc.nonce(from); {
	case nonce < next:
		vc.log.Verbo(
			"Nonce already used",
			zap.Uint64("tx", nonce),
			zap.Uint64("expect", next),
			zap.Stringer("tx_hash", t.tx.Hash()),
		)
		return discardTx, nil
	case nonce > next:
		vc.log.Verbo(
			"Future nonce",
			zap.Uint64("tx", nonce),
			zap.Uint64("next", next),
			zap.Stringer("tx_hash", t.tx.Hash()),
		)
		return delayTx, nil
	}

	price := uint256.NewInt(uint64(vc.gasClock.gasPrice()))
	if tx.GasFeeCap().Cmp(price.ToBig()) < 0 {
		vc.log.Verbo(
			"Gas-fee cap below base fee",
			zap.Stringer("cap", tx.GasFeeCap()),
			zap.Stringer("base_fee", price),
			zap.Stringer("tx_hash", t.tx.Hash()),
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
			zap.Stringer("cost", cost),
			zap.Stringer("balance", vc.balance(from)),
			zap.Stringer("tx_hash", t.tx.Hash()),
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
