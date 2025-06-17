package worstcase

import (
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
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

func newDB(tb testing.TB) *state.StateDB {
	tb.Helper()
	db, err := state.New(types.EmptyRootHash, state.NewDatabase(rawdb.NewMemoryDatabase()), nil)
	require.NoError(tb, err, "state.New([empty root], [fresh memory db])")
	return db
}

func newTxIncluder(tb testing.TB) (*TransactionIncluder, *state.StateDB) {
	tb.Helper()
	db := newDB(tb)
	return NewTxIncluder(
		db, params.MergedTestChainConfig,
		gastime.New(0, 1e6, 0),
		5, 2,
	), db
}

func TestNonContextualTransactionRejection(t *testing.T) {
	key, err := crypto.GenerateKey()
	require.NoError(t, err, "libevm/crypto.GenerateKey()")
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	tests := []struct {
		name       string
		stateSetup func(*state.StateDB)
		tx         types.TxData
		wantErrIs  error
	}{
		{
			name: "nil_err",
			stateSetup: func(db *state.StateDB) {
				db.SetBalance(eoa, uint256.NewInt(params.TxGas))
			},
			tx: &types.LegacyTx{
				Nonce:    0,
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				To:       &common.Address{},
			},
			wantErrIs: nil,
		},
		{
			name: "nonce_too_low",
			stateSetup: func(db *state.StateDB) {
				db.SetNonce(eoa, 1)
			},
			tx: &types.LegacyTx{
				Nonce: 0,
				Gas:   params.TxGas,
				To:    &common.Address{},
			},
			wantErrIs: core.ErrNonceTooLow,
		},
		{
			name: "nonce_too_high",
			stateSetup: func(db *state.StateDB) {
				db.SetNonce(eoa, 1)
			},
			tx: &types.LegacyTx{
				Nonce: 2,
				Gas:   params.TxGas,
				To:    &common.Address{},
			},
			wantErrIs: core.ErrNonceTooHigh,
		},
		{
			name: "exceed_max_init_code_size",
			tx: &types.LegacyTx{
				To:   nil, // i.e. contract creation
				Data: make([]byte, params.MaxInitCodeSize+1),
				Gas:  250_000, // cover intrinsic gas
			},
			wantErrIs: core.ErrMaxInitCodeSizeExceeded,
		},
		{
			name: "not_cover_intrinsic_gas",
			tx: &types.LegacyTx{
				Gas: params.TxGas - 1,
				To:  &common.Address{},
			},
			wantErrIs: core.ErrIntrinsicGas,
		},
		{
			name: "gas_price_too_low",
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(0),
				To:       &common.Address{},
			},
			wantErrIs: core.ErrFeeCapTooLow,
		},
		{
			name: "insufficient_funds_for_gas",
			stateSetup: func(db *state.StateDB) {
				db.SetBalance(eoa, uint256.NewInt(params.TxGas-1))
			},
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				To:       &common.Address{},
			},
			wantErrIs: core.ErrInsufficientFunds,
		},
		{
			name: "insufficient_funds_for_gas_and_value",
			stateSetup: func(db *state.StateDB) {
				db.SetBalance(eoa, uint256.NewInt(params.TxGas))
			},
			tx: &types.LegacyTx{
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
				Value:    big.NewInt(1),
				To:       &common.Address{},
			},
			wantErrIs: core.ErrInsufficientFunds,
		},
		{
			name: "blob_tx_not_supported",
			tx: &types.BlobTx{
				Gas: params.TxGas,
			},
			wantErrIs: core.ErrTxTypeNotSupported,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc, db := newTxIncluder(t)
			require.NoError(t, inc.StartBlock(&types.Header{
				Number: big.NewInt(0),
			}, 1e6), "StartBlock(0, t=0)")
			if tt.stateSetup != nil {
				tt.stateSetup(db)
			}
			tx := types.MustSignNewTx(key, types.NewCancunSigner(inc.config.ChainID), tt.tx)
			require.ErrorIs(t, inc.Include(tx), tt.wantErrIs)
		})
	}
}

func TestContextualTransactionRejection(t *testing.T) {
	// TODO(arr4n) test rejection of transactions in the context of other
	// transactions, e.g. exhausting balance, gas price increasing, etc.
}
