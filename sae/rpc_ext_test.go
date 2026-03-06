// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/assert"
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
	toBig := big.NewInt
	toHex := func(x int64) *hexutil.Big {
		return (*hexutil.Big)(toBig(x))
	}
	minimumPrice := &price{
		GasTip: toHex(params.Wei),
		GasFee: toHex(3 * params.Wei),
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
				Slow:   newPrice(toBig(tip*.95), toBig(2*baseFee)),
				Normal: newPrice(toBig(tip), toBig(2*baseFee)),
				Fast:   newPrice(toBig(tip*1.05), toBig(2*baseFee)),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tip := new(big.Int).SetUint64(test.tip)
			baseFee := new(big.Int).SetUint64(test.baseFee)
			got := newPriceOptions(tip, baseFee)
			assert.Equalf(t, test.want, got, "newPriceOptions(%s, %v)", tip, baseFee)
		})
	}
}

func TestSuggestPriceOptions(t *testing.T) {
	ctx, sut := newSUT(t, 0)
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   (*priceOptions)(nil),
	})

	sut.runConsensusLoop(t)

	// This just asserts the round-tripping of the priceOptions through the RPC.
	// See [TestNewPriceOptions] for behavioral tests.
	tip, err := sut.rawVM.apiBackend.SuggestGasTipCap(t.Context())
	require.NoErrorf(t, err, "SuggestGasTipCap()")
	baseFee := sut.rawVM.last.accepted.Load().WorstCaseBounds().LatestEndTime.BaseFee().ToBig()
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_suggestPriceOptions",
		want:   newPriceOptions(tip, baseFee),
	})
}
