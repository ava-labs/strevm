// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"crypto/ecdsa"
	"math"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/ethtest"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
)

type SUT struct {
	*State
	Genesis *blocks.Block
	Hooks   *hookstest.Stub
}

const (
	initialGasTarget = 1_000_000
	initialExcess    = 60_303_807 // Maximum excess that results in gas price of 1
)

func newSUT(tb testing.TB, alloc types.GenesisAlloc) SUT {
	tb.Helper()

	db, cache, _ := ethtest.NewEmptyStateDB(tb)
	config := saetest.ChainConfig()

	genesis := blockstest.NewGenesis(
		tb,
		db,
		config,
		alloc,
		blockstest.WithGasTarget(initialGasTarget),
		blockstest.WithGasExcess(initialExcess),
	)
	hooks := &hookstest.Stub{
		Target: initialGasTarget,
	}
	s, err := NewState(hooks, config, cache, genesis)
	require.NoError(tb, err, "NewState()")

	return SUT{
		State:   s,
		Genesis: genesis,
		Hooks:   hooks,
	}
}

const (
	targetToMaxBlockSize = gastime.TargetToRate * maxGasSecondsPerBlock
	initialMaxBlockSize  = initialGasTarget * targetToMaxBlockSize
)

type (
	Op           = hook.Op
	AccountDebit = hook.AccountDebit
)

func TestMultipleBlocks(t *testing.T) {
	var (
		eoa          = common.Address{0x01}
		eoaNoBalance = common.Address{0x02}
	)
	sut := newSUT(t, types.GenesisAlloc{
		eoa: {
			Balance: new(big.Int).SetUint64(math.MaxUint64),
		},
	})

	state := sut.State
	lastHash := sut.Genesis.Hash()

	const importedAmount = 10
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
					name: "include_small_operation",
					op: Op{
						Gas:       gas.Gas(params.TxGas),
						GasFeeCap: *uint256.NewInt(1),
					},
					wantErr: nil,
				},
				{
					name: "would_exceed_limit",
					op: Op{
						Gas:       gas.Gas(initialMaxBlockSize - params.TxGas + 1),
						GasFeeCap: *uint256.NewInt(1),
					},
					wantErr: core.ErrGasLimitReached,
				},
				{
					name: "fill_block",
					op: Op{
						Gas:       gas.Gas(initialMaxBlockSize - params.TxGas),
						GasFeeCap: *uint256.NewInt(1),
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
						Gas:       1,
						GasFeeCap: *uint256.NewInt(2),
						Mint: map[common.Address]uint256.Int{
							eoaNoBalance: *uint256.NewInt(importedAmount),
						},
					},
					wantErr: nil,
				},
				{
					name: "imported_funds_insufficient",
					op: Op{
						Gas:       1,
						GasFeeCap: *uint256.NewInt(2),
						Burn: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Amount: *uint256.NewInt(importedAmount + 1),
							},
						},
					},
					wantErr: core.ErrInsufficientFunds,
				},
				{
					name: "spend_imported_funds",
					op: Op{
						Gas:       1,
						GasFeeCap: *uint256.NewInt(2),
						Burn: map[common.Address]AccountDebit{
							eoaNoBalance: {
								Amount: *uint256.NewInt(importedAmount),
							},
						},
					},
					wantErr: nil,
				},
			},
		},
		{
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
			*sut.Hooks = *block.hooks
		}
		header := &types.Header{
			ParentHash: lastHash,
			Number:     big.NewInt(int64(i)),
			Time:       block.time,
		}
		got, err := state.StartBlock(header)
		require.NoErrorf(t, err, "StartBlock(%d)", i)
		assert.Equalf(t, block.wantBaseFee, got, "base fee returned by StartBlock(%d)")
		assert.Equalf(t, block.wantBaseFee, state.BaseFee(), "base fee after StartBlock(%d)", i)
		assert.Equalf(t, block.wantGasLimit, state.GasLimit(), "gas limit after StartBlock(%d)", i)
		if t.Failed() {
			t.FailNow()
		}

		for _, op := range block.ops {
			gotErr := state.Apply(op.op)
			require.ErrorIsf(t, gotErr, op.wantErr, "Apply(%s) error", op.name)
		}

		require.NoError(t, state.FinishBlock(), "FinishBlock()")
		lastHash = header.Hash()
	}
}

func TestTransactionValidation(t *testing.T) {
	// Test parameters from https://eips.ethereum.org/EIPS/eip-3607
	eip3607Key, err := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	require.NoError(t, err, "crypto.HexToECDSA() for eip3607Key")
	eip3607EOA := crypto.PubkeyToAddress(eip3607Key.PublicKey)
	eip3607Alloc := types.Account{
		Balance: big.NewInt(params.Ether),
		Nonce:   0,
		Code:    common.Hex2Bytes("B0B0FACE"),
	}

	defaultKey, err := crypto.GenerateKey()
	require.NoError(t, err, "libevm/crypto.GenerateKey()")
	defaultEOA := crypto.PubkeyToAddress(defaultKey.PublicKey)

	// The referenced clause definitions are documented by Geth in
	// [core.StateTransition.transitionDb].
	tests := []struct {
		name    string
		nonce   uint64
		balance uint64
		tx      types.TxData
		key     *ecdsa.PrivateKey
		wantErr error
	}{
		// Clause 1: the nonce of the message caller is correct
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

		// Clause 2: caller has enough balance to cover transaction fee(gaslimit * gasprice)
		// Clause 6: caller has enough balance to cover asset transfer for **topmost** call
		{
			name: "negative_gas_price",
			tx: &types.LegacyTx{
				GasPrice: big.NewInt(-1),
				Gas:      params.TxGas,
				To:       &common.Address{},
			},
			wantErr: txpool.ErrUnderpriced,
		},
		{
			name: "negative_value",
			tx: &types.LegacyTx{
				GasPrice: big.NewInt(1),
				Gas:      params.TxGas,
				To:       &common.Address{},
				Value:    big.NewInt(-1),
			},
			wantErr: txpool.ErrNegativeValue,
		},
		{
			name: "cost_overflow",
			tx: &types.LegacyTx{
				GasPrice: new(big.Int).Lsh(big.NewInt(1), 256-1),
				Gas:      params.TxGas,
				To:       &common.Address{},
			},
			wantErr: errCostOverflow,
		},
		{
			name: "insufficient_funds",
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				To:       &common.Address{},
				Value:    big.NewInt(0),
			},
			wantErr: core.ErrInsufficientFunds,
		},

		// Clause 3: the amount of gas required is available in the block
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

		// Clause 4: the purchased gas is enough to cover intrinsic usage
		{
			name: "not_cover_intrinsic_gas",
			tx: &types.LegacyTx{
				To:  &common.Address{},
				Gas: params.TxGas - 1,
			},
			wantErr: core.ErrIntrinsicGas,
		},

		// Clause 5 (there is no overflow when calculating intrinsic gas) is not
		// tested because it requires constructing such a large transaction that
		// the test would OOM.

		// EIP-1559: onchain gas auction
		{
			name: "gas_fee_cap_very_high",
			tx: &types.DynamicFeeTx{
				GasTipCap: big.NewInt(1),
				GasFeeCap: new(big.Int).Lsh(big.NewInt(1), 256),
				Gas:       params.TxGas,
				To:        &common.Address{},
			},
			wantErr: core.ErrFeeCapVeryHigh,
		},
		{
			name: "gas_tip_cap_very_high",
			tx: &types.DynamicFeeTx{
				GasTipCap: new(big.Int).Lsh(big.NewInt(1), 256),
				GasFeeCap: big.NewInt(1),
				Gas:       params.TxGas,
				To:        &common.Address{},
			},
			wantErr: core.ErrTipVeryHigh,
		},
		{
			name: "gas_tip_above_fee_cap",
			tx: &types.DynamicFeeTx{
				GasTipCap: big.NewInt(2),
				GasFeeCap: big.NewInt(1),
				Gas:       params.TxGas,
				To:        &common.Address{},
			},
			wantErr: core.ErrTipAboveFeeCap,
		},
		{
			name: "gas_fee_cap_too_low",
			tx: &types.DynamicFeeTx{
				GasTipCap: big.NewInt(0),
				GasFeeCap: big.NewInt(0),
				Gas:       params.TxGas,
				To:        &common.Address{},
			},
			wantErr: core.ErrFeeCapTooLow,
		},

		// EIP-3607: reject transactions from non-EOAs
		{
			name: "sender_not_eoa",
			tx: &types.LegacyTx{
				GasPrice: big.NewInt(1),
				Gas:      params.TxGas,
				To:       &common.Address{},
			},
			key:     eip3607Key,
			wantErr: core.ErrSenderNoEOA,
		},

		// EIP-3860: limit init code size
		{
			name: "exceed_max_init_code_size",
			tx: &types.LegacyTx{
				Gas:  250_000, // cover intrinsic gas
				To:   nil,     // contract creation
				Data: make([]byte, params.MaxInitCodeSize+1),
			},
			wantErr: core.ErrMaxInitCodeSizeExceeded,
		},

		// Unsupported transaction types
		{
			name: "blob_tx_not_supported",
			tx: &types.BlobTx{
				Gas: params.TxGas,
			},
			wantErr: core.ErrTxTypeNotSupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sut := newSUT(t, types.GenesisAlloc{
				defaultEOA: {
					Nonce:   tt.nonce,
					Balance: new(big.Int).SetUint64(tt.balance),
				},
				eip3607EOA: eip3607Alloc,
			})
			state := sut.State

			header := &types.Header{
				ParentHash: sut.Genesis.Hash(),
				Number:     big.NewInt(0),
			}
			_, err := state.StartBlock(header)
			require.NoErrorf(t, err, "StartBlock()")

			key := defaultKey
			if tt.key != nil {
				key = tt.key
			}
			tx := types.MustSignNewTx(key, types.NewCancunSigner(state.config.ChainID), tt.tx)
			got, err := state.ApplyTx(tx)
			require.ErrorIsf(t, err, tt.wantErr, "ApplyTx() error")
			_ = got // TODO(arr4n)
		})
	}
}

// Test that non-consecutive blocks are sanity checked.
func TestStartBlockNonConsecutiveBlocks(t *testing.T) {
	sut := newSUT(t, nil)
	state := sut.State
	genesisHash := sut.Genesis.Hash()

	_, err := state.StartBlock(&types.Header{
		ParentHash: genesisHash,
	})
	require.NoError(t, err, "StartBlock()")

	_, err = state.StartBlock(&types.Header{
		ParentHash: genesisHash, // Should be the previously provided header's hash
	})
	require.ErrorIs(t, err, errNonConsecutiveBlocks, "non-consecutive StartBlock()")
}

// Test that filling the queue eventually prevents new blocks from being added.
func TestStartBlockQueueFull(t *testing.T) {
	sut := newSUT(t, nil)
	state := sut.State
	lastHash := sut.Genesis.Hash()

	// Fill the queue with the minimum amount of gas to prevent additional
	// blocks.
	for number, gas := range []gas.Gas{initialMaxBlockSize, initialMaxBlockSize, 1} {
		h := &types.Header{
			ParentHash: lastHash,
			Number:     big.NewInt(int64(number)),
		}
		_, err := state.StartBlock(h)
		require.NoError(t, err, "StartBlock()")

		err = state.Apply(Op{
			Gas:       gas,
			GasFeeCap: *uint256.NewInt(2),
		})
		require.NoError(t, err, "Apply()")

		require.NoError(t, state.FinishBlock(), "FinishBlock()")

		lastHash = h.Hash()
	}

	_, err := state.StartBlock(&types.Header{
		ParentHash: lastHash,
		Number:     big.NewInt(3),
	})
	require.ErrorIs(t, err, ErrQueueFull, "StartBlock() with full queue")
}

// Test that changing the target can cause the queue to be treated as full.
func TestStartBlockQueueFullDueToTargetChanges(t *testing.T) {
	sut := newSUT(t, nil)
	state := sut.State

	sut.Hooks.Target = 1 // applied after the first block
	h := &types.Header{
		ParentHash: sut.Genesis.Hash(),
		Number:     big.NewInt(0),
	}
	_, err := state.StartBlock(h)
	require.NoError(t, err, "StartBlock()")

	err = state.Apply(Op{
		Gas:       initialMaxBlockSize,
		GasFeeCap: *uint256.NewInt(1),
	})
	require.NoError(t, err, "Apply()")

	require.NoError(t, state.FinishBlock(), "FinishBlock()")

	_, err = state.StartBlock(&types.Header{
		ParentHash: h.Hash(),
		Number:     big.NewInt(1),
	})
	require.ErrorIs(t, err, ErrQueueFull, "StartBlock() with full queue")
}
