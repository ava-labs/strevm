// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saetest

import (
	"crypto/ecdsa"
	"encoding/binary"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/ethtest"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

// A Wallet manages a set of private keys (suitable only for tests) and nonces
// to sign transactions.
type Wallet struct {
	accounts []*account
	signer   types.Signer
}

type account struct {
	key   *ecdsa.PrivateKey
	nonce uint64
}

// NewUNSAFEWallet returns a new wallet with the specified number of accounts.
// Private keys are generated deterministically.
func NewUNSAFEWallet(tb testing.TB, accounts uint, signer types.Signer) *Wallet {
	tb.Helper()

	w := &Wallet{
		accounts: make([]*account, accounts),
		signer:   signer,
	}
	for i := range accounts {
		seed := binary.BigEndian.AppendUint64(nil, uint64(i))
		key := ethtest.UNSAFEDeterministicPrivateKey(tb, seed)
		w.accounts[i] = &account{key: key}
	}
	return w
}

// Addresses returns all addresses managed by the wallet.
func (w *Wallet) Addresses() []common.Address {
	addrs := make([]common.Address, len(w.accounts))
	for i, a := range w.accounts {
		addrs[i] = crypto.PubkeyToAddress(a.key.PublicKey)
	}
	return addrs
}

// SetNonceAndSign overrides the nonce in the `data` with the next one for the
// account, then signs and returns the transaction. The wallet's record of the
// account nonce begins at zero and increments after every successful call to
// this method.
func (w *Wallet) SetNonceAndSign(tb testing.TB, account int, data types.TxData) *types.Transaction {
	tb.Helper()

	acc := w.accounts[account]

	switch d := data.(type) {
	case *types.LegacyTx:
		d.Nonce = acc.nonce
	case *types.AccessListTx:
		d.Nonce = acc.nonce
	case *types.DynamicFeeTx:
		d.Nonce = acc.nonce
	default:
		tb.Fatalf("Unsupported transaction type: %T", d)
	}

	tx, err := types.SignNewTx(acc.key, w.signer, data)
	require.NoError(tb, err, "types.SignNewTx(...)")

	acc.nonce++
	return tx
}

// SetSigner updates the signer used to sign transactions.
func (w *Wallet) SetSigner(s types.Signer) {
	w.signer = s
}

// DecrementNonce decrements the nonce of the specified account. This is useful
// for retrying transactions with updated parameters.
func (w *Wallet) DecrementNonce(tb testing.TB, account int) {
	tb.Helper()
	a := w.accounts[account]
	require.NotZerof(tb, a.nonce, "Nonce of account [%d] MUST be non-zero to decrement", account)
	a.nonce--
}

// MaxAllocFor returns a genesis allocation with [MaxUint256] as the balance for
// all addresses provided.
func MaxAllocFor(addrs ...common.Address) types.GenesisAlloc {
	alloc := make(types.GenesisAlloc)
	for _, a := range addrs {
		alloc[a] = types.Account{
			Balance: new(uint256.Int).SetAllOne().ToBig(),
		}
	}
	return alloc
}
