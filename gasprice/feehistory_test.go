// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/saetest"
)

func TestFeeHistory(t *testing.T) {
	cases := []struct {
		maxCallBlock uint64
		maxBlock     uint64
		count        uint64
		last         rpc.BlockNumber
		percent      []float64
		expFirst     uint64
		expCount     int
		expErr       error
	}{
		// Standard libevm tests
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: nil, expFirst: 21, expCount: 10, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: []float64{0, 10}, expFirst: 21, expCount: 10, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: []float64{20, 10}, expFirst: 0, expCount: 0, expErr: errInvalidPercentile},
		{maxCallBlock: 0, maxBlock: 1000, count: 1000000000, last: 30, percent: nil, expFirst: 0, expCount: 31, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 1000000000, last: rpc.LatestBlockNumber, percent: nil, expFirst: 0, expCount: 33, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 40, percent: nil, expFirst: 0, expCount: 0, expErr: errRequestBeyondHead},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 40, percent: nil, expFirst: 0, expCount: 0, expErr: errRequestBeyondHead},
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: rpc.LatestBlockNumber, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: 32, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},
		// In SAE backend, `pending` resolves to accepted head (not head+1).
		{maxCallBlock: 0, maxBlock: 1000, count: 1, last: rpc.PendingBlockNumber, percent: nil, expFirst: 32, expCount: 1, expErr: nil},
		// With count=2, pending spans [head-1, head].
		{maxCallBlock: 0, maxBlock: 1000, count: 2, last: rpc.PendingBlockNumber, percent: nil, expFirst: 31, expCount: 2, expErr: nil},
		// Same behavior when "pending" mode is enabled in test matrix.
		{maxCallBlock: 0, maxBlock: 1000, count: 2, last: rpc.PendingBlockNumber, percent: nil, expFirst: 31, expCount: 2, expErr: nil},
		// Reward percentiles should not alter the pending range semantics.
		{maxCallBlock: 0, maxBlock: 1000, count: 2, last: rpc.PendingBlockNumber, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},

		// Modified tests
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: rpc.LatestBlockNumber, percent: nil, expFirst: 31, expCount: 2, expErr: nil},    // apply block lookback limits even if only headers required
		{maxCallBlock: 0, maxBlock: 10, count: 10, last: 30, percent: nil, expFirst: 23, expCount: 8, expErr: nil},                       // limit lookback based on maxHistory from latest block
		{maxCallBlock: 0, maxBlock: 33, count: 1000000000, last: 10, percent: nil, expFirst: 0, expCount: 11, expErr: nil},               // handle truncation edge case
		{maxCallBlock: 0, maxBlock: 2, count: 10, last: 20, percent: nil, expFirst: 0, expCount: 0, expErr: errBeyondHistoricalLimit},    // query behind historical limit
		{maxCallBlock: 10, maxBlock: 30, count: 100, last: rpc.LatestBlockNumber, percent: nil, expFirst: 23, expCount: 10, expErr: nil}, // ensure [MaxCallBlockHistory] is honored
	}
	for i, c := range cases {
		kc := saetest.NewUNSAFEKeyChain(t, 1)
		addr := kc.Addresses()[0]
		signer := types.LatestSigner(testChainConfig)
		tip := big.NewInt(1 * params.GWei)
		backend := newTestBackend(t, 32, func(i int, b *testBlockGen) {
			b.SetCoinbase(common.Address{1})

			baseFee := b.BaseFee()
			feeCap := new(big.Int).Add(baseFee, tip)

			tx := kc.SignTx(t, signer, 0, &types.DynamicFeeTx{
				ChainID:   testChainConfig.ChainID,
				Nonce:     b.TxNonce(addr),
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasFeeCap: feeCap,
				GasTipCap: tip,
				Data:      []byte{},
			})
			b.AddTx(tx)
		})
		oracleOpts := make([]OracleOption, 0, 2)
		if c.maxCallBlock != 0 {
			maxCallBlockOpt, err := WithMaxCallBlockHistory(c.maxCallBlock)
			require.NoError(t, err)
			oracleOpts = append(oracleOpts, maxCallBlockOpt)
		}
		if c.maxBlock != 0 {
			maxBlockOpt, err := WithMaxBlockHistory(c.maxBlock)
			require.NoError(t, err)
			oracleOpts = append(oracleOpts, maxBlockOpt)
		}
		oracle, err := NewOracle(backend, oracleOpts...)
		require.NoError(t, err)

		first, reward, baseFee, ratio, err := oracle.FeeHistory(context.Background(), c.count, c.last, c.percent)
		expReward := c.expCount
		if len(c.percent) == 0 {
			expReward = 0
		}
		expBaseFee := c.expCount

		if first.Uint64() != c.expFirst {
			t.Fatalf("Test case %d: first block mismatch, want %d, got %d", i, c.expFirst, first)
		}
		if len(reward) != expReward {
			t.Fatalf("Test case %d: reward array length mismatch, want %d, got %d", i, expReward, len(reward))
		}
		if len(baseFee) != expBaseFee {
			t.Fatalf("Test case %d: baseFee array length mismatch, want %d, got %d", i, expBaseFee, len(baseFee))
		}
		if len(ratio) != c.expCount {
			t.Fatalf("Test case %d: gasUsedRatio array length mismatch, want %d, got %d", i, c.expCount, len(ratio))
		}
		if err != c.expErr && !errors.Is(err, c.expErr) {
			t.Fatalf("Test case %d: error mismatch, want %v, got %v", i, c.expErr, err)
		}
	}
}
