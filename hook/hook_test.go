// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package hook

import (
	"math"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOp_ApplyTo(t *testing.T) {
	var (
		eoa         = common.Address{0x00}
		eoaMaxNonce = common.Address{0x01}
	)
	type account struct {
		address common.Address
		nonce   uint64
		balance *uint256.Int
	}
	tests := []struct {
		name         string
		op           *Op
		wantAccounts []account
		wantErr      error
	}{
		{
			name: "mint_to_eoa",
			op: &Op{
				To: map[common.Address]uint256.Int{
					eoa: *uint256.NewInt(1_000_000),
				},
			},
			wantAccounts: []account{
				{
					address: eoa,
					nonce:   0,
					balance: uint256.NewInt(1_000_000),
				},
				{
					address: eoaMaxNonce,
					nonce:   math.MaxUint64,
					balance: uint256.NewInt(0),
				},
			},
		},
		{
			name: "move_from_eoa_to_eoaMaxNonce",
			op: &Op{
				From: map[common.Address]AccountDebit{
					eoa: {
						Nonce:  0,
						Amount: *uint256.NewInt(100_000),
					},
				},
				To: map[common.Address]uint256.Int{
					eoaMaxNonce: *uint256.NewInt(100_000),
				},
			},
			wantAccounts: []account{
				{
					address: eoa,
					nonce:   1,
					balance: uint256.NewInt(900_000),
				},
				{
					address: eoaMaxNonce,
					nonce:   math.MaxUint64,
					balance: uint256.NewInt(100_000),
				},
			},
		},
		{
			name: "burn_all_funds",
			op: &Op{
				From: map[common.Address]AccountDebit{
					eoa: {
						Nonce:  1,
						Amount: *uint256.NewInt(900_000),
					},
					eoaMaxNonce: {
						Nonce:  math.MaxUint64,
						Amount: *uint256.NewInt(100_000),
					},
				},
			},
			wantAccounts: []account{
				{
					address: eoa,
					nonce:   2,
					balance: uint256.NewInt(0),
				},
				{
					address: eoaMaxNonce,
					nonce:   math.MaxUint64,
					balance: uint256.NewInt(0),
				},
			},
		},
		{
			name: "invalid_mint",
			op: &Op{
				From: map[common.Address]AccountDebit{
					eoa: {
						Nonce:  2,
						Amount: *uint256.NewInt(100_000),
					},
				},
			},
			wantErr: core.ErrInsufficientFunds,
		},
	}

	db, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(t, err, "state.New([empty root], [fresh memory db])")
	db.SetNonce(eoaMaxNonce, math.MaxUint64)
	for _, tt := range tests {
		require.ErrorIs(t, tt.op.ApplyTo(db), tt.wantErr, "ApplyTo %s", tt.name)
		for _, acct := range tt.wantAccounts {
			assert.Equalf(t, acct.nonce, db.GetNonce(acct.address), "nonce of account %s after %s", acct.address, tt.name)
			assert.Equalf(t, acct.balance, db.GetBalance(acct.address), "balance of account %s after %s", acct.address, tt.name)
		}
		if t.Failed() {
			t.FailNow()
		}
	}
}
