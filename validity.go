package sae

import (
	"fmt"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/holiman/uint256"
)

type validityChecker struct {
	db *state.StateDB

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
	ql := vc.queueLength + gas.Gas(t.tx.Gas())
	if ql > stateRootDelaySeconds*maxGasPerSecond*lambda {
		return queueFull, nil
	}
	vc.queueLength = ql

	tx := t.tx
	from := t.from

	switch nonce, next := tx.Nonce(), vc.nonce(from); {
	case nonce < next:
		return discardTx, nil
	case nonce > next:
		return delayTx, nil
	}

	gasCost := new(uint256.Int).Mul(
		uint256.NewInt(uint64(vc.gasClock.gasPrice())),
		uint256.NewInt(tx.Gas()),
	)
	cost := gasCost.Add(gasCost, uint256.MustFromBig(tx.Value()))

	if !vc.trySpend(from, cost) {
		return delayTx, nil
	}
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
