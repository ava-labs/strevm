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
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
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
	hooks := &hookstest.Stub{}
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
}

// func TestNonContextualTransactionRejection(t *testing.T) {
// 	key, err := crypto.GenerateKey()
// 	require.NoError(t, err, "libevm/crypto.GenerateKey()")
// 	eoa := crypto.PubkeyToAddress(key.PublicKey)

// 	tests := []struct {
// 		name       string
// 		stateSetup func(*state.StateDB)
// 		tx         types.TxData
// 		wantErrIs  error
// 	}{
// 		{
// 			name: "nil_err",
// 			stateSetup: func(db *state.StateDB) {
// 				db.SetBalance(eoa, uint256.NewInt(params.TxGas))
// 			},
// 			tx: &types.LegacyTx{
// 				Nonce:    0,
// 				Gas:      params.TxGas,
// 				GasPrice: big.NewInt(1),
// 				To:       &common.Address{},
// 			},
// 			wantErrIs: nil,
// 		},
// 		{
// 			name: "nonce_too_low",
// 			stateSetup: func(db *state.StateDB) {
// 				db.SetNonce(eoa, 1)
// 			},
// 			tx: &types.LegacyTx{
// 				Nonce: 0,
// 				Gas:   params.TxGas,
// 				To:    &common.Address{},
// 			},
// 			wantErrIs: core.ErrNonceTooLow,
// 		},
// 		{
// 			name: "insufficient_funds_for_gas",
// 			stateSetup: func(db *state.StateDB) {
// 				db.SetBalance(eoa, uint256.NewInt(params.TxGas-1))
// 			},
// 			tx: &types.LegacyTx{
// 				Gas:      params.TxGas,
// 				GasPrice: big.NewInt(1),
// 				To:       &common.Address{},
// 			},
// 			wantErrIs: core.ErrInsufficientFunds,
// 		},
// 		{
// 			name: "insufficient_funds_for_gas_and_value",
// 			stateSetup: func(db *state.StateDB) {
// 				db.SetBalance(eoa, uint256.NewInt(params.TxGas))
// 			},
// 			tx: &types.LegacyTx{
// 				Gas:      params.TxGas,
// 				GasPrice: big.NewInt(1),
// 				Value:    big.NewInt(1),
// 				To:       &common.Address{},
// 			},
// 			wantErrIs: core.ErrInsufficientFunds,
// 		},
// 	}

// 	for _, tt := range tests {
// 		t.Run(tt.name, func(t *testing.T) {
// 			inc, db := newTxIncluder(t)
// 			require.NoError(t, inc.StartBlock(&types.Header{
// 				Number: big.NewInt(0),
// 			}, 1e6), "StartBlock(0, t=0)")
// 			if tt.stateSetup != nil {
// 				tt.stateSetup(db)
// 			}
// 			tx := types.MustSignNewTx(key, types.NewCancunSigner(inc.config.ChainID), tt.tx)
// 			require.ErrorIs(t, inc.ApplyTx(tx), tt.wantErrIs)
// 		})
// 	}
// }

func TestContextualTransactionRejection(t *testing.T) {
	// TODO(arr4n) test rejection of transactions in the context of other
	// transactions, e.g. exhausting balance, gas price increasing, etc.
}
