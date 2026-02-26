// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"
	"time"

	"github.com/arr4n/shed/testerr"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
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

func TestStateAtBlockAndTransaction(t *testing.T) {
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

	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	registerBlockingPrecompile(t, blockingPrecompile)

	genesis := sut.lastAcceptedBlock(t)

	// Once a block is settled, its ancestors are only accessible from the database.
	onDisk := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))

	settled := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))
	vmTime.advanceToSettle(ctx, t, settled)

	// Build after settlement so it stays in memory.
	executed := sut.runConsensusLoop(t, createTx(t, recv), createTx(t, recv))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	pending := sut.runConsensusLoop(t, createTx(t, blockingPrecompile))

	be := sut.rawVM.apiBackend

	// Verify that accepting `executed` settled its ancestors, evicting
	// genesis and onDisk from the in-memory block map while retaining
	// settled as the LastSettled reference.
	_, inMem := sut.rawVM.blocks.Load(genesis.Hash())
	require.False(t, inMem, "genesis should have been evicted from vm.blocks")
	_, inMem = sut.rawVM.blocks.Load(onDisk.Hash())
	require.False(t, inMem, "onDisk should have been evicted from vm.blocks")
	_, inMem = sut.rawVM.blocks.Load(settled.Hash())
	require.True(t, inMem, "settled should still be in vm.blocks as LastSettled reference")
	_, inMem = sut.rawVM.blocks.Load(executed.Hash())
	require.True(t, inMem, "executed should still be in vm.blocks")
	_, inMem = sut.rawVM.blocks.Load(pending.Hash())
	require.True(t, inMem, "pending should still be in vm.blocks")

	t.Run("StateAtBlock", func(t *testing.T) {
		tests := []struct {
			name    string
			block   *blocks.Block
			wantErr testerr.Want
		}{
			{name: "genesis", block: genesis},
			{name: "on_disk", block: onDisk},
			{name: "settled", block: settled},
			{name: "executed", block: executed},
			{name: "unexecuted", block: pending, wantErr: testerr.Contains("execution results not yet available")},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				sdb, release, err := be.StateAtBlock(ctx, tt.block.EthBlock(), 0, nil, false, false)
				t.Logf("%T.StateAtBlock(ctx, block %d)", be, tt.block.Height())
				if diff := testerr.Diff(err, tt.wantErr); diff != "" {
					t.Fatalf("StateAtBlock(...) %s", diff)
				}
				if tt.wantErr != nil {
					return
				}
				defer release()
				assert.NotNilf(t, sdb, "%T.StateAtBlock() returned nil StateDB", be)
			})
		}
	})

	t.Run("StateAtTransaction", func(t *testing.T) {
		tests := []struct {
			name    string
			block   *blocks.Block
			txIndex int
			wantErr testerr.Want
		}{
			{name: "first_tx_on_disk", block: onDisk, txIndex: 0},
			{name: "second_tx_on_disk", block: onDisk, txIndex: 1},
			{name: "first_tx_settled", block: settled, txIndex: 0},
			{name: "second_tx_settled", block: settled, txIndex: 1},
			{name: "first_tx_executed", block: executed, txIndex: 0},
			{name: "second_tx_executed", block: executed, txIndex: 1},
			{name: "genesis", block: genesis, txIndex: 0, wantErr: testerr.Contains("no transactions in genesis")},
			{name: "out_of_range", block: executed, txIndex: 5, wantErr: testerr.Contains("out of range")},
			{name: "negative_index", block: executed, txIndex: -1, wantErr: testerr.Contains("out of range")},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				msg, blockCtx, sdb, release, err := be.StateAtTransaction(ctx, tt.block.EthBlock(), tt.txIndex, 0)
				t.Logf("%T.StateAtTransaction(ctx, block %d, txIndex %d)", be, tt.block.Height(), tt.txIndex)
				if diff := testerr.Diff(err, tt.wantErr); diff != "" {
					t.Fatalf("StateAtTransaction(...) %s", diff)
				}
				if tt.wantErr != nil {
					return
				}
				defer release()
				assert.NotNilf(t, msg, "%T.StateAtTransaction() returned nil Message", be)
				assert.NotNilf(t, sdb, "%T.StateAtTransaction() returned nil StateDB", be)
				assert.NotZerof(t, blockCtx.BlockNumber, "%T.StateAtTransaction() returned zero BlockNumber in context", be)
			})
		}
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
