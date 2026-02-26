// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImmediateReceipts(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	blocking := common.Address{'b', 'l', 'o', 'c', 'k'}
	registerBlockingPrecompile(t, blocking)

	var txs []*types.Transaction
	for _, to := range []*common.Address{&zeroAddr, &blocking} {
		txs = append(txs, sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       to,
			Gas:      params.TxGas,
			GasPrice: big.NewInt(1),
		}))
	}
	notBlocked := txs[0]

	b := sut.runConsensusLoop(t, txs...)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_getTransactionReceipt",
		args:   []any{notBlocked.Hash()},
		want: &types.Receipt{
			Status:            types.ReceiptStatusSuccessful,
			EffectiveGasPrice: big.NewInt(1),
			GasUsed:           params.TxGas,
			CumulativeGasUsed: params.TxGas,
			Logs:              []*types.Log{},
			TxHash:            notBlocked.Hash(),
			BlockHash:         b.Hash(),
			BlockNumber:       b.Number(),
		},
	})
	assert.Falsef(t, b.Executed(), "%T.Executed()", b)
}

func TestCallGetReceiptBeforeAcceptance(t *testing.T) {
	t.Parallel()

	ctx, sut := newSUT(t, 1)

	tx := sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
		To:       &zeroAddr,
		Gas:      params.TxGas,
		GasPrice: big.NewInt(1),
	})
	sut.mustSendTx(t, tx)
	sut.requireInMempool(t, tx.Hash())

	t.Run("start_fetching_receipt_before_inclusion", func(t *testing.T) {
		t.Parallel()
		got, err := sut.TransactionReceipt(ctx, tx.Hash())
		require.NoErrorf(t, err, "%T.TransactionReceipt()", sut.Client)
		require.Equalf(t, tx.Hash(), got.TxHash, "%T.TxHash", got)
	})

	t.Run("accept_block_while_waiting_on_receipt", func(t *testing.T) {
		t.Parallel()
		time.Sleep(500 * time.Millisecond) // <------------- Noteworthy
		sut.runConsensusLoop(t)
	})
}
