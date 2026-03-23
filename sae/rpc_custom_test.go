// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/cmputils"
	saeparams "github.com/ava-labs/strevm/params"
	saerpc "github.com/ava-labs/strevm/sae/rpc"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saetest/escrow"
)

func TestGetChainConfig(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_getChainConfig",
		want:   *saetest.ChainConfig(),
	})
}

func TestBaseFee(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_baseFee",
		want:   hexBig(params.InitialBaseFee),
	})

	b := sut.runConsensusLoop(t)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_baseFee",
		want:   (*hexutil.Big)(b.WorstCaseBounds().LatestEndTime.BaseFee().ToBig()),
	})
}

func TestSuggestPriceOptions(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	// Before any blocks with worst-case bounds, the base fee falls back to the
	// genesis base fee and the tip defaults to the minimum (no txs yet).
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   saerpc.NewPriceOptions(big.NewInt(params.Wei), big.NewInt(2*params.InitialBaseFee)),
	})

	b := sut.runConsensusLoop(t)

	// This just asserts the round-tripping of the PriceOptions through the RPC.
	// See testing of [saerpc.NewPriceOptions] for behavioral tests.
	tip, err := sut.rawVM.GethRPCBackends().SuggestGasTipCap(t.Context())
	require.NoErrorf(t, err, "SuggestGasTipCap()")
	doubleBaseFee := b.WorstCaseBounds().LatestEndTime.BaseFee().ToBig()
	doubleBaseFee.Lsh(doubleBaseFee, 1)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   saerpc.NewPriceOptions(tip, doubleBaseFee),
	})
}

func TestNewAcceptedTransactions(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	hashes := make(chan common.Hash, 1)
	{
		sub, err := sut.rpcClient.EthSubscribe(ctx, hashes, "newAcceptedTransactions")
		require.NoErrorf(t, err, "EthSubscribe(newAcceptedTransactions)")
		t.Cleanup(sub.Unsubscribe)
	}
	txs := make(chan *ethapi.RPCTransaction, 1)
	{
		sub, err := sut.rpcClient.EthSubscribe(ctx, txs, "newAcceptedTransactions", true)
		require.NoErrorf(t, err, "EthSubscribe(newAcceptedTransactions, true)")
		t.Cleanup(sub.Unsubscribe)
	}

	tx := sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
		To:       &zeroAddr,
		Gas:      params.TxGas,
		GasPrice: big.NewInt(1),
	})
	block := sut.runConsensusLoop(t, tx)

	t.Run("hash_only", func(t *testing.T) {
		require.Equalf(t, tx.Hash(), <-hashes, "accepted tx hash")
	})
	t.Run("full_tx", func(t *testing.T) {
		want := ethapi.NewRPCTransaction(tx, block.Hash(), block.NumberU64(), block.BuildTime(), 0, block.Header().BaseFee, saetest.ChainConfig())
		if diff := cmp.Diff(want, <-txs, cmputils.HexutilBigs(), cmputils.NilSlicesAreEmpty[hexutil.Bytes]()); diff != "" {
			t.Errorf("full tx diff (-want +got):\n%s", diff)
		}
	})
}

func TestCallDetailed(t *testing.T) {
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
	_, ok := sut.rawVM.consensusCritical.Load(b.Hash())
	require.Falsef(t, ok, "%T[%#x] still in VM memory", b, b.Hash())

	callArgs := map[string]any{
		"to":   escrowAddr,
		"data": hexutil.Encode(escrow.CallDataForBalance(recv)),
	}

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
			sut.testRPC(ctx, t, rpcTest{
				method: "eth_callDetailed",
				args:   []any{callArgs, tt.num.String()},
				want: saerpc.DetailedExecutionResult{
					UsedGas:    23675,
					ReturnData: uint256.NewInt(val).PaddedBytes(32),
				},
			})
		})
	}

	// Calling withdraw() from the zero address (which has no escrowed
	// balance) reverts.
	t.Run("reverting_call", func(t *testing.T) {
		sut.testRPC(ctx, t, rpcTest{
			method: "eth_callDetailed",
			args: []any{
				map[string]any{
					"to":   escrowAddr,
					"data": hexutil.Encode(escrow.CallDataForWithdraw()),
				},
				rpc.LatestBlockNumber.String(),
			},
			want: saerpc.DetailedExecutionResult{
				UsedGas: 23451,
				ErrCode: 3,
				Err:     "execution reverted",
				// InsufficientBalance(uint256) with balance=0
				ReturnData: common.FromHex("0xb5a1ab380000000000000000000000000000000000000000000000000000000000000000"),
			},
		})
	})
}
