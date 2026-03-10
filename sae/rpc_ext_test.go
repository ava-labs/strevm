// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/params"
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
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_baseFee",
		want:   (*hexutil.Big)(nil),
	})

	b := sut.runConsensusLoop(t)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_baseFee",
		want:   (*hexutil.Big)(b.WorstCaseBounds().LatestEndTime.BaseFee().ToBig()),
	})
}

func TestNewPriceOptions(t *testing.T) {
	minimumPrice := &price{
		GasTip: hexBig(params.Wei),
		GasFee: hexBig(2 * params.Wei),
	}
	const (
		tip     = 500
		baseFee = 100
	)
	tests := []struct {
		name    string
		tip     uint64
		baseFee uint64
		want    *priceOptions
	}{
		{
			name:    "minimum",
			tip:     params.Wei,
			baseFee: params.Wei,
			want: &priceOptions{
				Slow:   minimumPrice,
				Normal: minimumPrice,
				Fast:   minimumPrice,
			},
		},
		{
			name:    "percentages",
			tip:     tip,
			baseFee: baseFee,
			want: &priceOptions{
				Slow:   newPrice(big.NewInt(tip*.95), big.NewInt(baseFee)),
				Normal: newPrice(big.NewInt(tip), big.NewInt(baseFee)),
				Fast:   newPrice(big.NewInt(tip*1.05), big.NewInt(baseFee)),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tip := new(big.Int).SetUint64(test.tip)
			baseFee := new(big.Int).SetUint64(test.baseFee)
			got := newPriceOptions(tip, baseFee)
			require.Equalf(t, test.want, got, "newPriceOptions(%s, %v)", tip, baseFee)
		})
	}
}

func TestSuggestPriceOptions(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   (*priceOptions)(nil),
	})

	b := sut.runConsensusLoop(t)

	// This just asserts the round-tripping of the priceOptions through the RPC.
	// See [TestNewPriceOptions] for behavioral tests.
	tip, err := sut.rawVM.apiBackend.SuggestGasTipCap(t.Context())
	require.NoErrorf(t, err, "SuggestGasTipCap()")
	doubleBaseFee := b.WorstCaseBounds().LatestEndTime.BaseFee().ToBig()
	doubleBaseFee.Lsh(doubleBaseFee, 1)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   newPriceOptions(tip, doubleBaseFee),
	})
}
