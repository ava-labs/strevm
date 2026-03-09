// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"
	"time"

	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/tracers/logger"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest/escrow"
)

// txTraceResult mirrors the unexported tracers.txTraceResult.
type txTraceResult struct {
	TxHash common.Hash             `json:"txHash"`
	Result *logger.ExecutionResult `json:"result,omitempty"`
	Error  string                  `json:"error,omitempty"`
}

func TestTraceContractInteraction(t *testing.T) {
	opt, _ := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, opt)

	escrowAddr := crypto.CreateAddress(sut.wallet.Addresses()[0], 0)
	recv := common.Address{'r', 'e', 'c', 'v'}
	const depositVal = 42

	sign := sut.wallet.SetNonceAndSign
	deployTx := sign(t, 0, &types.LegacyTx{
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CreationCode(),
	})
	depositTx := sign(t, 0, &types.LegacyTx{
		To:       &escrowAddr,
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CallDataToDeposit(recv),
		Value:    big.NewInt(depositVal),
	})

	b := sut.runConsensusLoop(t, deployTx, depositTx)
	require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	for _, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Status", r)
	}

	t.Run("debug_traceTransaction", func(t *testing.T) {
		var got logger.ExecutionResult
		err := sut.CallContext(ctx, &got, "debug_traceTransaction", depositTx.Hash())
		require.NoError(t, err)
		assert.False(t, got.Failed)
		assert.Greater(t, got.Gas, params.TxGas)
		require.NotEmpty(t, got.StructLogs)
		assert.True(t, hasOp(got.StructLogs, "SSTORE"), "deposit must write to storage")
		assert.True(t, hasOp(got.StructLogs, "SLOAD"), "deposit must read existing balance")
	})

	t.Run("debug_traceBlockByNumber", func(t *testing.T) {
		var got []txTraceResult
		err := sut.CallContext(ctx, &got, "debug_traceBlockByNumber", hexutil.Uint64(b.Height()))
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, deployTx.Hash(), got[0].TxHash)
		require.NotNil(t, got[0].Result)
		assert.False(t, got[0].Result.Failed)
		require.NotEmpty(t, got[0].Result.StructLogs)

		assert.Equal(t, depositTx.Hash(), got[1].TxHash)
		require.NotNil(t, got[1].Result)
		assert.False(t, got[1].Result.Failed)
		require.NotEmpty(t, got[1].Result.StructLogs)
		assert.True(t, hasOp(got[1].Result.StructLogs, "SSTORE"), "deposit must write to storage")
	})
}

func TestEthCall(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, opt)

	deploy := &types.LegacyTx{
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CreationCode(),
	}

	escrowAddr := crypto.CreateAddress(sut.wallet.Addresses()[0], 0)
	recv := common.Address{'r', 'e', 'c', 'v'}
	const val = 42
	deposit := &types.LegacyTx{
		To:       &escrowAddr,
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CallDataToDeposit(recv),
		Value:    big.NewInt(val),
	}

	sign := sut.wallet.SetNonceAndSign
	b := sut.runConsensusLoop(t, sign(t, 0, deploy), sign(t, 0, deposit))
	require.Len(t, b.Transactions(), 2, "tx count")
	require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	for _, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Status", r)
	}

	vmTime.advanceToSettle(ctx, t, b)
	for range 2 {
		bb := sut.runConsensusLoop(t)
		vmTime.advanceToSettle(ctx, t, bb)
	}
	_, ok := sut.rawVM.blocks.Load(b.Hash())
	require.Falsef(t, ok, "%T[%#x] still in VM memory", b, b.Hash())

	tests := []struct {
		name string
		num  rpc.BlockNumber
	}{
		{
			name: "block_in_memory",
			num:  rpc.LatestBlockNumber,
		},
		{
			name: "block_on_disk",
			num:  rpc.BlockNumber(b.Number().Int64()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := ethereum.CallMsg{
				To:   &escrowAddr,
				Data: escrow.CallDataForBalance(recv),
			}

			got, err := sut.CallContract(ctx, msg, big.NewInt(int64(tt.num)))
			t.Logf("%T.CallContract(%+v, %d)", sut.Client, msg, tt.num) // avoids having to repeat in failure messages
			require.NoError(t, err)
			assert.Equal(t, uint256.NewInt(val).PaddedBytes(32), got)
		})
	}
}

// hasOp reports whether any [logger.StructLogRes] in logs has the given opcode.
func hasOp(logs []logger.StructLogRes, op string) bool {
	for _, l := range logs {
		if l.Op == op {
			return true
		}
	}
	return false
}
