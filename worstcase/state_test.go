// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"math"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook/hookstest"
)

const initialGasTarget = 1_000_000

func newDB(tb testing.TB) *state.StateDB {
	tb.Helper()
	db, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(tb, err, "state.New([empty root], [fresh memory db])")
	return db
}

type SUT struct {
	*State
	DB    *state.StateDB
	Hooks *hookstest.Stub
}

func newSUT(tb testing.TB) SUT {
	tb.Helper()
	db := newDB(tb)
	hooks := &hookstest.Stub{
		Target: initialGasTarget,
	}
	return SUT{
		State: NewState(
			hooks,
			params.MergedTestChainConfig,
			db,
			gastime.New(0, initialGasTarget, 0),
		),
		DB:    db,
		Hooks: hooks,
	}
}

const targetToMaxBlockSize = gastime.TargetToRate * rateToMaxBlockSize

func TestState(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err, "libevm/crypto.GenerateKey()")
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	state := newSUT(t)
	state.DB.SetBalance(eoa, uint256.NewInt(math.MaxUint64))

	eoaMaxNonce := common.Address{0x00}
	state.DB.SetNonce(eoaMaxNonce, math.MaxUint64)
	state.DB.SetBalance(eoaMaxNonce, uint256.NewInt(math.MaxUint64))

	eoaNoBalance := common.Address{0x01}
	type op struct {
		name    string
		tx      types.TxData
		op      *Op
		wantErr error
	}
	blocks := []struct {
		hooks        *hookstest.Stub
		wantGasLimit uint64
		wantBaseFee  *uint256.Int
		ops          []op
	}{
		{
			wantGasLimit: initialGasTarget * targetToMaxBlockSize,
			wantBaseFee:  uint256.NewInt(1),
			ops: []op{
				{
					name: "not_cover_intrinsic_gas",
					tx: &types.LegacyTx{
						To:  &common.Address{},
						Gas: params.TxGas - 1,
					},
					wantErr: core.ErrIntrinsicGas,
				},
				{
					name: "exceed_max_init_code_size",
					tx: &types.LegacyTx{
						Gas:  250_000, // cover intrinsic gas
						To:   nil,     // contract creation
						Data: make([]byte, params.MaxInitCodeSize+1),
					},
					wantErr: core.ErrMaxInitCodeSizeExceeded,
				},
				{
					name: "blob_tx_not_supported",
					tx: &types.BlobTx{
						Gas: params.TxGas,
					},
					wantErr: core.ErrTxTypeNotSupported,
				},
				{
					name: "cost_overflow",
					tx: &types.LegacyTx{
						Nonce:    0,
						GasPrice: new(big.Int).Lsh(big.NewInt(1), 256-1),
						Gas:      params.TxGas,
						To:       &eoa,
						Value:    big.NewInt(10),
					},
					wantErr: errCostOverflow,
				},
				{
					name: "gas_price_too_low",
					tx: &types.LegacyTx{
						GasPrice: big.NewInt(0),
						Gas:      params.TxGas,
						To:       &common.Address{},
					},
					wantErr: core.ErrFeeCapTooLow,
				},
				{
					name: "nonce_too_high",
					tx: &types.LegacyTx{
						Nonce:    1,
						GasPrice: big.NewInt(1),
						Gas:      params.TxGas,
						To:       &common.Address{},
					},
					wantErr: core.ErrNonceTooHigh,
				},
				{
					name: "insufficient_funds_for_gas",
					tx: &types.LegacyTx{
						Gas:      params.TxGas,
						GasPrice: new(big.Int).SetUint64(math.MaxUint64),
						To:       &common.Address{},
						Value:    big.NewInt(0),
					},
					wantErr: core.ErrInsufficientFunds,
				},
				{
					name: "insufficient_funds_for_value",
					tx: &types.LegacyTx{
						Gas:      params.TxGas,
						GasPrice: big.NewInt(1),
						To:       &common.Address{},
						Value:    new(big.Int).SetUint64(math.MaxUint64),
					},
					wantErr: core.ErrInsufficientFunds,
				},
				{
					name: "include_transfer",
					tx: &types.LegacyTx{
						Nonce:    0,
						GasPrice: big.NewInt(1),
						Gas:      params.TxGas,
						To:       &eoa,
						Value:    big.NewInt(10),
					},
					wantErr: nil,
				},
				{
					name: "nonce_too_low",
					tx: &types.LegacyTx{
						Nonce:    0,
						GasPrice: big.NewInt(1),
						Gas:      params.TxGas,
						To:       &common.Address{},
					},
					wantErr: core.ErrNonceTooLow,
				},
				{
					name: "block_too_full",
					tx: &types.LegacyTx{
						Nonce:    1,
						GasPrice: big.NewInt(1),
						Gas:      initialGasTarget*targetToMaxBlockSize - params.TxGas + 1,
						To:       &common.Address{},
					},
					wantErr: core.ErrGasLimitReached,
				},
				{
					name: "full_block",
					tx: &types.LegacyTx{
						Nonce:    1,
						GasPrice: big.NewInt(1),
						Gas:      initialGasTarget*targetToMaxBlockSize - params.TxGas,
						To:       &common.Address{},
					},
					wantErr: nil,
				},
			},
		},
		{
			wantGasLimit: initialGasTarget * targetToMaxBlockSize,
			wantBaseFee:  uint256.NewInt(1),
			ops: []op{
				{
					name: "max_nonce",
					op: &Op{
						Gas:      1,
						GasPrice: *uint256.NewInt(1),
						From: map[common.Address]AccountDebit{
							eoaMaxNonce: {
								Nonce:  math.MaxUint64,
								Amount: *uint256.NewInt(1),
							},
						},
					},
					wantErr: core.ErrNonceMax,
				},
				{
					name: "import",
					op: &Op{
						Gas:      1,
						GasPrice: *uint256.NewInt(1),
						From: map[common.Address]AccountDebit{
							eoa: {
								Nonce:  2,
								Amount: *uint256.NewInt(1),
							},
						},
						To: map[common.Address]uint256.Int{
							eoaNoBalance: *uint256.NewInt(10),
						},
					},
					wantErr: nil,
				},
				{
					name: "imported_insufficient_funds",
					op: &Op{
						Gas:      initialGasTarget*targetToMaxBlockSize - 1,
						GasPrice: *uint256.NewInt(1),
						From: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Nonce:  0,
								Amount: *uint256.NewInt(11),
							},
						},
					},
					wantErr: core.ErrInsufficientFunds,
				},
				{
					name: "spend_imported_funds",
					op: &Op{
						Gas:      initialGasTarget*targetToMaxBlockSize - 1,
						GasPrice: *uint256.NewInt(1),
						From: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Nonce:  0,
								Amount: *uint256.NewInt(10),
							},
						},
					},
					wantErr: nil,
				},
			},
		},
		{
			wantGasLimit: initialGasTarget * targetToMaxBlockSize,
			wantBaseFee:  uint256.NewInt(1),
			ops: []op{
				{
					name: "full_block",
					tx: &types.LegacyTx{
						Nonce:    3,
						GasPrice: big.NewInt(1),
						Gas:      initialGasTarget * targetToMaxBlockSize,
						To:       &common.Address{},
					},
					wantErr: nil,
				},
			},
		},
	}
	for i, block := range blocks {
		if block.hooks != nil {
			*state.Hooks = *block.hooks
		}
		header := &types.Header{
			Number: big.NewInt(int64(i)),
		}
		require.NoErrorf(t, state.StartBlock(header), "StartBlock(%d)", i)
		require.Equalf(t, block.wantBaseFee, state.BaseFee(), "base fee after StartBlock(%d)", i)
		require.Equalf(t, block.wantGasLimit, state.GasLimit(), "gas limit after StartBlock(%d)", i)

		for _, op := range block.ops {
			if op.tx != nil {
				tx := types.MustSignNewTx(key, types.NewCancunSigner(state.config.ChainID), op.tx)
				gotErr := state.ApplyTx(tx)
				require.ErrorIsf(t, gotErr, op.wantErr, "ApplyTx(%s) error", op.name)
			}
			if op.op != nil {
				gotErr := state.Apply(*op.op)
				require.ErrorIsf(t, gotErr, op.wantErr, "Apply(%s) error", op.name)
			}
		}

		require.NoError(t, state.FinishBlock(), "FinishBlock()")
	}

	// Test that nonconsecutive blocks are disallowed.
	{
		header := &types.Header{
			Number: big.NewInt(int64(len(blocks) + 1)),
		}
		err := state.StartBlock(header)
		require.ErrorIs(t, err, errNonConsecutiveBlocks, "nonconsecutive StartBlock()")
	}

	// Test that starting a new block fails if the queue is full.
	{
		header := &types.Header{
			Number: big.NewInt(int64(len(blocks))),
		}
		err := state.StartBlock(header)
		require.ErrorIsf(t, err, errQueueFull, "StartBlock(%d)", len(blocks))
	}
}
