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
	unblock := registerBlockingPrecompile(t, blocking)

	var txs []*types.Transaction
	for _, to := range []*common.Address{&zeroAddr, &blocking} {
		txs = append(txs, sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       to,
			Gas:      params.TxGas,
			GasPrice: big.NewInt(1),
		}))
	}
	notBlocked := txs[0]
	blocked := txs[1]

	b := sut.createAndAcceptBlock(t, txs[:]...)

	// DO NOT MERGE: use Eventually() once the regular receipt method has been
	// implemented so a non-existent receipt doesn't panic.
	time.Sleep(time.Second)

	sut.testRPC(ctx, t, rpcTest{
		method: "eth_getTransactionReceipt",
		args:   []any{notBlocked.Hash()},
		want: &types.Receipt{
			Status:            types.ReceiptStatusSuccessful,
			GasUsed:           params.TxGas,
			CumulativeGasUsed: params.TxGas,
			TxHash:            notBlocked.Hash(),
			BlockHash:         b.Hash(),
			BlockNumber:       b.Number(),
		},
	})

	// DO NOT MERGE: see above
	time.Sleep(time.Second)
	_ = blocked
	assert.False(t, b.Executed())

	// Although this isn't strictly necessary, it demonstrates that the blocking
	// was for the expected reason.
	unblock()
	require.NoError(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()")
}
