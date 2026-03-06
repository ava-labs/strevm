// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/saetest"
)

func TestGetChainConfig(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_getChainConfig",
		want:   *saetest.ChainConfig(),
	})
}

func TestBaseFee(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	b := sut.runConsensusLoop(t)
	bounds := b.WorstCaseBounds()
	require.NotNil(t, bounds, "WorstCaseBounds()")

	sut.testRPC(ctx, t, rpcTest{
		method: "eth_baseFee",
		want:   (*hexutil.Big)(bounds.LatestEndTime.BaseFee().ToBig()),
	})
}

func TestSuggestPriceOptions(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	b := sut.runConsensusLoop(t)
	bounds := b.WorstCaseBounds()
	require.NotNil(t, bounds, "WorstCaseBounds()")

	var got priceOptions
	require.NoError(t, sut.CallContext(ctx, &got, "eth_suggestPriceOptions"), "eth_suggestPriceOptions")

	baseFee := bounds.LatestEndTime.BaseFee().ToBig()
	doubleBaseFee := new(big.Int).Lsh(baseFee, 1)

	for _, tc := range []struct {
		name string
		p    *price
	}{
		{"slow", got.Slow},
		{"normal", got.Normal},
		{"fast", got.Fast},
	} {
		require.NotNil(t, tc.p, "%s price", tc.name)
		tip := tc.p.GasTip.ToInt()
		require.Positive(t, tip.Sign(), "%s: tip must be positive", tc.name)
		fee := tc.p.GasFee.ToInt()
		wantFee := new(big.Int).Add(doubleBaseFee, tip)
		require.Zero(t, fee.Cmp(wantFee), "%s: gasFee = 2*baseFee + tip", tc.name)
	}

	// Slow tip <= normal tip <= fast tip.
	require.LessOrEqual(t, got.Slow.GasTip.ToInt().Cmp(got.Normal.GasTip.ToInt()), 0, "slow tip <= normal tip")
	require.LessOrEqual(t, got.Normal.GasTip.ToInt().Cmp(got.Fast.GasTip.ToInt()), 0, "normal tip <= fast tip")
}
