// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"math"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook/hookstest"
)

type SUT struct {
	*State
	DB    *state.StateDB
	Hooks *hookstest.Stub
}

const (
	initialGasTarget = 1_000_000
	initialExcess    = 60_303_807 // Maximum excess that results in gas price of 1
)

func newSUT(tb testing.TB) SUT {
	tb.Helper()
	hooks := &hookstest.Stub{
		Target: initialGasTarget,
	}
	db, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(tb, err, "state.New([empty root], [fresh memory db])")
	return SUT{
		State: NewState(
			hooks,
			params.MergedTestChainConfig,
			db,
			gastime.New(0, initialGasTarget, initialExcess),
		),
		DB:    db,
		Hooks: hooks,
	}
}

const (
	targetToMaxBlockSize = gastime.TargetToRate * rateToMaxBlockSize
	initialMaxBlockSize  = initialGasTarget * targetToMaxBlockSize
)

func TestMultipleBlocks(t *testing.T) {
	var (
		eoa          = common.Address{0x01}
		eoaNoBalance = common.Address{0x02}
	)
	state := newSUT(t)
	state.DB.SetBalance(eoa, uint256.NewInt(math.MaxUint64))
	type op struct {
		name    string
		op      Op
		wantErr error
	}
	blocks := []struct {
		hooks        *hookstest.Stub
		time         uint64
		wantGasLimit uint64
		wantBaseFee  *uint256.Int
		ops          []op
	}{
		{
			hooks: &hookstest.Stub{
				Target: 2 * initialGasTarget, // Will double the target _after_ this block.
			},
			wantGasLimit: initialMaxBlockSize,
			wantBaseFee:  uint256.NewInt(1),
			ops: []op{
				{
					name: "include_small operation",
					op: Op{
						Gas:      gas.Gas(params.TxGas),
						GasPrice: *uint256.NewInt(1),
					},
					wantErr: nil,
				},
				{
					name: "block_too_full",
					op: Op{
						Gas:      gas.Gas(initialMaxBlockSize - params.TxGas + 1),
						GasPrice: *uint256.NewInt(1),
					},
					wantErr: core.ErrGasLimitReached,
				},
				{
					name: "block_full",
					op: Op{
						Gas:      gas.Gas(initialMaxBlockSize - params.TxGas),
						GasPrice: *uint256.NewInt(1),
					},
					wantErr: nil,
				},
			},
		},
		{
			hooks: &hookstest.Stub{
				Target: initialGasTarget, // Restore the target _after_ this block.
			},
			wantGasLimit: 2 * initialMaxBlockSize,
			wantBaseFee:  uint256.NewInt(2),
			ops: []op{
				{
					name: "import",
					op: Op{
						Gas:      1,
						GasPrice: *uint256.NewInt(2),
						To: map[common.Address]uint256.Int{
							eoaNoBalance: *uint256.NewInt(10),
						},
					},
					wantErr: nil,
				},
				{
					name: "imported_funds_insufficient",
					op: Op{
						Gas:      1,
						GasPrice: *uint256.NewInt(2),
						From: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Amount: *uint256.NewInt(11),
							},
						},
					},
					wantErr: core.ErrInsufficientFunds,
				},
				{
					name: "spend_imported_funds",
					op: Op{
						Gas:      1,
						GasPrice: *uint256.NewInt(2),
						From: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Amount: *uint256.NewInt(10),
							},
						},
					},
					wantErr: nil,
				},
			},
		},
		{
			hooks: &hookstest.Stub{
				Target: initialGasTarget, // Restore the target _after_ this block.
			},
			// We have currently included slightly over 10s worth of gas. We
			// should increase the time by that same amount to restore the base
			// fee.
			time:         21,
			wantGasLimit: initialMaxBlockSize,
			wantBaseFee:  uint256.NewInt(1),
		},
	}
	for i, block := range blocks {
		if block.hooks != nil {
			*state.Hooks = *block.hooks
		}
		header := &types.Header{
			Number: big.NewInt(int64(i)),
			Time:   block.time,
		}
		require.NoErrorf(t, state.StartBlock(header), "StartBlock(%d)", i)
		require.Equalf(t, block.wantBaseFee, state.BaseFee(), "base fee after StartBlock(%d)", i)
		require.Equalf(t, block.wantGasLimit, state.GasLimit(), "gas limit after StartBlock(%d)", i)

		for _, op := range block.ops {
			gotErr := state.Apply(op.op)
			require.ErrorIsf(t, gotErr, op.wantErr, "Apply(%s) error", op.name)
		}

		require.NoError(t, state.FinishBlock(), "FinishBlock()")
	}
}

func TestTransactionValidation(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err, "libevm/crypto.GenerateKey()")
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	tests := []struct {
		name    string
		nonce   uint64
		balance uint64
		tx      types.TxData
		wantErr error
	}{
		{
			name: "blob_tx_not_supported",
			tx: &types.BlobTx{
				Gas: params.TxGas,
			},
			wantErr: core.ErrTxTypeNotSupported,
		},
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
			name: "gas_price_overflow",
			tx: &types.LegacyTx{
				GasPrice: new(big.Int).Lsh(big.NewInt(1), 256),
				Gas:      params.TxGas,
				To:       &common.Address{},
				Value:    big.NewInt(10),
			},
			wantErr: core.ErrFeeCapVeryHigh,
		},
		{
			name: "cost_overflow",
			tx: &types.LegacyTx{
				GasPrice: new(big.Int).Lsh(big.NewInt(1), 256-1),
				Gas:      params.TxGas,
				To:       &common.Address{},
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
			name:  "nonce_too_low",
			nonce: 1,
			tx: &types.LegacyTx{
				GasPrice: big.NewInt(1),
				Gas:      params.TxGas,
				To:       &common.Address{},
			},
			wantErr: core.ErrNonceTooLow,
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
			name:  "max_nonce",
			nonce: math.MaxUint64,
			tx: &types.LegacyTx{
				Nonce:    math.MaxUint64,
				GasPrice: big.NewInt(1),
				Gas:      params.TxGas,
				To:       &common.Address{},
			},
			wantErr: core.ErrNonceMax,
		},
		{
			name: "insufficient_funds_for_gas",
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				To:       &common.Address{},
				Value:    big.NewInt(0),
			},
			wantErr: core.ErrInsufficientFunds,
		},
		{
			name:    "insufficient_funds_for_value",
			balance: params.TxGas,
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				To:       &common.Address{},
				Value:    big.NewInt(1),
			},
			wantErr: core.ErrInsufficientFunds,
		},
		{
			name:    "gas_limit_exceeded",
			balance: initialMaxBlockSize,
			tx: &types.LegacyTx{
				GasPrice: big.NewInt(1),
				Gas:      initialMaxBlockSize + 1,
				To:       &common.Address{},
			},
			wantErr: txpool.ErrGasLimit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newSUT(t)
			state.DB.SetNonce(eoa, tt.nonce)
			state.DB.SetBalance(eoa, uint256.NewInt(tt.balance))

			header := &types.Header{
				Number: big.NewInt(0),
			}
			require.NoErrorf(t, state.StartBlock(header), "StartBlock()")

			tx := types.MustSignNewTx(key, types.NewCancunSigner(state.config.ChainID), tt.tx)
			gotErr := state.ApplyTx(tx)
			require.ErrorIsf(t, gotErr, tt.wantErr, "ApplyTx() error")
		})
	}
}

// Test that non-consecutive blocks are sanity checked.
func TestStartBlockNonConsecutiveBlocks(t *testing.T) {
	state := newSUT(t)

	err := state.StartBlock(&types.Header{
		Number: big.NewInt(0),
	})
	require.NoError(t, err, "StartBlock()")

	err = state.StartBlock(&types.Header{
		Number: big.NewInt(2), // Should be 1 to be consecutive
	})
	require.ErrorIs(t, err, errNonConsecutiveBlocks, "nonconsecutive StartBlock()")
}

// Test that filling the queue eventually prevents new blocks from being added.
func TestStartBlockQueueFull(t *testing.T) {
	state := newSUT(t)

	// Fill the queue with the minimum amount of gas to prevent additional
	// blocks.
	for number, gas := range []gas.Gas{initialMaxBlockSize, initialMaxBlockSize, 1} {
		err := state.StartBlock(&types.Header{
			Number: big.NewInt(int64(number)),
		})
		require.NoError(t, err, "StartBlock()")

		err = state.Apply(Op{
			Gas:      gas,
			GasPrice: *uint256.NewInt(2),
		})
		require.NoError(t, err, "Apply()")

		err = state.FinishBlock()
		require.NoError(t, err, "FinishBlock()")
	}

	err := state.StartBlock(&types.Header{
		Number: big.NewInt(3),
	})
	require.ErrorIs(t, err, errQueueFull, "StartBlock() with full queue")
}

// Test that changing the target can cause the queue to be treated as full.
func TestStartBlockQueueFullDueToTargetChanges(t *testing.T) {
	state := newSUT(t)

	state.Hooks.Target = 1
	err := state.StartBlock(&types.Header{
		Number: big.NewInt(0),
	})
	require.NoError(t, err, "StartBlock()")

	err = state.Apply(Op{
		Gas:      initialMaxBlockSize,
		GasPrice: *uint256.NewInt(1),
	})
	require.NoError(t, err, "Apply()")

	err = state.FinishBlock()
	require.NoError(t, err, "FinishBlock()")

	err = state.StartBlock(&types.Header{
		Number: big.NewInt(1),
	})
	require.ErrorIs(t, err, errQueueFull, "StartBlock() with full queue")
}
