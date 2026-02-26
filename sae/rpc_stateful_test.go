// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/arr4n/shed/testerr"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest/escrow"
)

// traceResult mirrors the subset of [tracers/logger.ExecutionResult] fields
// needed for assertions.
type traceResult struct {
	Gas         uint64           `json:"gas"`
	Failed      bool             `json:"failed"`
	ReturnValue string           `json:"returnValue"`
	StructLogs  []structLogEntry `json:"structLogs"`
}

// structLogEntry mirrors the subset of [tracers/logger.StructLogRes] fields
// needed for assertions.
type structLogEntry struct {
	Op string `json:"op"`
}

// txTraceResult mirrors the subset of [tracers.txTraceResult] fields needed
// for assertions.
type txTraceResult struct {
	TxHash common.Hash  `json:"txHash"`
	Result *traceResult `json:"result,omitempty"`
	Error  string       `json:"error,omitempty"`
}

func TestStateTracing(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, opt)

	recv := common.Address{'r', 'e', 'c', 'v'}
	createTx := func(t *testing.T, to common.Address) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &to,
			Value:     big.NewInt(100),
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}

	genesis := sut.lastAcceptedBlock(t)

	// Once a block is settled, its ancestors are only accessible from the database.
	onDisk := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))

	settled := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))
	vmTime.advanceToSettle(ctx, t, settled)

	// Build after settlement so it stays in memory.
	executed := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	requireBlockTraces := func(t *testing.T, got []txTraceResult, block *blocks.Block) {
		t.Helper()
		require.Len(t, got, len(block.Transactions()), "trace result count")
		for i, tr := range got {
			assert.Equalf(t, block.Transactions()[i].Hash(), tr.TxHash, "txHash[%d]", i)
			require.NotNilf(t, tr.Result, "trace result[%d]", i)
			assert.Greaterf(t, tr.Result.Gas, uint64(0), "gas[%d]", i)
			assert.Falsef(t, tr.Result.Failed, "failed[%d]", i)
		}
	}

	t.Run("debug_traceBlockByNumber", func(t *testing.T) {
		tests := []struct {
			name    string
			block   *blocks.Block
			wantErr testerr.Want
		}{
			{name: "on_disk", block: onDisk},
			{name: "settled", block: settled},
			{name: "executed", block: executed},
			{name: "genesis", block: genesis, wantErr: testerr.Contains("genesis is not traceable")},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Logf("debug_traceBlockByNumber(%d)", tt.block.Height())
				var got []txTraceResult
				err := sut.CallContext(ctx, &got, "debug_traceBlockByNumber", hexutil.Uint64(tt.block.Height()))
				if diff := testerr.Diff(err, tt.wantErr); diff != "" {
					t.Fatalf("debug_traceBlockByNumber(...) %s", diff)
				}
				if tt.wantErr != nil {
					return
				}
				requireBlockTraces(t, got, tt.block)
			})
		}
	})

	t.Run("debug_traceBlockByHash", func(t *testing.T) {
		tests := []struct {
			name  string
			block *blocks.Block
		}{
			{name: "on_disk", block: onDisk},
			{name: "settled", block: settled},
			{name: "executed", block: executed},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Logf("debug_traceBlockByHash(%#x)", tt.block.Hash())
				var got []txTraceResult
				err := sut.CallContext(ctx, &got, "debug_traceBlockByHash", tt.block.Hash())
				require.NoError(t, err)
				requireBlockTraces(t, got, tt.block)
			})
		}
	})

	t.Run("debug_traceTransaction", func(t *testing.T) {
		tests := []struct {
			name  string
			block *blocks.Block
		}{
			{name: "on_disk", block: onDisk},
			{name: "settled", block: settled},
			{name: "executed", block: executed},
		}

		for _, tt := range tests {
			for i, tx := range tt.block.Transactions() {
				t.Run(fmt.Sprintf("%s/tx_%d", tt.name, i), func(t *testing.T) {
					t.Logf("debug_traceTransaction(%#x)", tx.Hash())
					var got traceResult
					err := sut.CallContext(ctx, &got, "debug_traceTransaction", tx.Hash())
					require.NoError(t, err)
					assert.Greater(t, got.Gas, uint64(0), "gas")
					assert.False(t, got.Failed, "failed")
				})
			}
		}
	})
}

// TestTraceContractInteraction complements [TestStateTracing] by tracing
// contract deployment and a state-modifying deposit call, verifying that
// the tracer produces correct EVM-level output (SSTORE, SLOAD, etc.).
func TestTraceContractInteraction(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
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

	// Settle and evict so the block is only accessible from disk.
	vmTime.advanceToSettle(ctx, t, b)
	for range 2 {
		bb := sut.runConsensusLoop(t)
		vmTime.advanceToSettle(ctx, t, bb)
	}
	hasOp := func(logs []structLogEntry, op string) bool {
		for _, l := range logs {
			if l.Op == op {
				return true
			}
		}
		return false
	}

	// Trace the deposit tx (txIndex=1). The tracer must replay the deploy
	// tx first via StateAtTransaction, then trace the deposit which writes
	// to contract storage (SSTORE) and emits the Deposit event (LOG1).
	t.Run("debug_traceTransaction", func(t *testing.T) {
		t.Logf("debug_traceTransaction(%#x) [deposit into escrow]", depositTx.Hash())
		var got traceResult
		err := sut.CallContext(ctx, &got, "debug_traceTransaction", depositTx.Hash())
		require.NoError(t, err)
		assert.False(t, got.Failed, "failed")
		assert.Greater(t, got.Gas, params.TxGas, "gas must exceed simple transfer cost")
		require.NotEmpty(t, got.StructLogs, "structLogs must not be empty for contract call")
		assert.True(t, hasOp(got.StructLogs, "SSTORE"), "deposit must write to storage")
		assert.True(t, hasOp(got.StructLogs, "SLOAD"), "deposit must read existing balance")
	})

	// Trace the full block. Both the deploy (tx 0) and deposit (tx 1)
	// should produce non-trivial traces.
	t.Run("debug_traceBlockByNumber", func(t *testing.T) {
		t.Logf("debug_traceBlockByNumber(%d) [deploy + deposit]", b.Height())
		var got []txTraceResult
		err := sut.CallContext(ctx, &got, "debug_traceBlockByNumber", hexutil.Uint64(b.Height()))
		require.NoError(t, err)
		require.Len(t, got, 2, "trace result count")

		deploy := got[0]
		assert.Equal(t, deployTx.Hash(), deploy.TxHash, "deploy txHash")
		require.NotNil(t, deploy.Result, "deploy trace result")
		assert.False(t, deploy.Result.Failed, "deploy failed")
		require.NotEmpty(t, deploy.Result.StructLogs, "deploy structLogs")

		deposit := got[1]
		assert.Equal(t, depositTx.Hash(), deposit.TxHash, "deposit txHash")
		require.NotNil(t, deposit.Result, "deposit trace result")
		assert.False(t, deposit.Result.Failed, "deposit failed")
		require.NotEmpty(t, deposit.Result.StructLogs, "deposit structLogs")
		assert.True(t, hasOp(deposit.Result.StructLogs, "SSTORE"), "deposit must write to storage")
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
