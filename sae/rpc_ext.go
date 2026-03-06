// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math/big"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
)

// customAPI implements Avalanche-custom RPCs. These are not part of the
// standard Ethereum JSON-RPC spec or in geth, but are exposed by Avalanche
// nodes for compatibility with tooling that depends on them (e.g. Core).
//
// Reference implementations live at:
// - https://github.com/ava-labs/avalanchego/blob/v1.14.1/graft/coreth/internal/ethapi/api_extra.go
// - https://github.com/ava-labs/avalanchego/blob/v1.14.1/graft/coreth/internal/ethapi/api.coreth.go
type customAPI struct {
	b *apiBackend
}

// GetChainConfig returns the chain configuration.
func (c *customAPI) GetChainConfig(ctx context.Context) *params.ChainConfig {
	return c.b.ChainConfig()
}

// BaseFee returns an upper-bound estimate of the base fee for the next block.
// It returns nil if the estimate is unavailable, matching coreth's behaviour.
func (c *customAPI) BaseFee(ctx context.Context) (*hexutil.Big, error) {
	return (*hexutil.Big)(c.b.EstimateNextBaseFee()), nil
}

// detailedExecutionResult is the response for eth_callDetailed.
type detailedExecutionResult struct {
	UsedGas    uint64        `json:"gas"`
	ErrCode    int           `json:"errCode"`
	Err        string        `json:"err"`
	ReturnData hexutil.Bytes `json:"returnData"`
}

// CallDetailed performs the same call as eth_call, but returns gas usage and
// error details instead of just the return data.
func (c *customAPI) CallDetailed(ctx context.Context, args any, blockNrOrHash rpc.BlockNumberOrHash, overrides any) (*detailedExecutionResult, error) {
	panic(errUnimplemented)
}

// price represents a single gas-price suggestion.
type price struct {
	GasTip *hexutil.Big `json:"maxPriorityFeePerGas"`
	GasFee *hexutil.Big `json:"maxFeePerGas"`
}

// priceOptions groups slow/normal/fast gas-price suggestions.
type priceOptions struct {
	Slow   *price `json:"slow"`
	Normal *price `json:"normal"`
	Fast   *price `json:"fast"`
}

// Tip-speed scaling constants, matching coreth's defaults.
// See github.com/ava-labs/avalanchego/blob/1a59a6f646ef/graft/coreth/plugin/evm/config/default_config.go#L84-L85
const (
	slowTipPct = 95
	fastTipPct = 105
)

var (
	minGasTip  = big.NewInt(1) // 1 Wei floor for the slow tier, matching coreth.
	bigHundred = big.NewInt(100)
)

// SuggestPriceOptions returns gas-price suggestions at three speed tiers.
// Each tier contains a tip and a total fee cap (2*baseFee + tip), following
// the same approach as the coreth reference implementation.
func (c *customAPI) SuggestPriceOptions(ctx context.Context) (*priceOptions, error) {
	baseFee := c.b.EstimateNextBaseFee()
	if baseFee == nil {
		return nil, nil
	}

	tip, err := c.b.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}

	slowTip := math.BigMax(scaleTip(tip, slowTipPct), minGasTip) // Floor matching coreth's minGasTip.
	fastTip := scaleTip(tip, fastTipPct)

	doubleBaseFee := new(big.Int).Lsh(baseFee, 1)
	return &priceOptions{
		Slow:   newPrice(slowTip, doubleBaseFee),
		Normal: newPrice(tip, doubleBaseFee),
		Fast:   newPrice(fastTip, doubleBaseFee),
	}, nil
}

// scaleTip returns tip * pct / 100.
func scaleTip(tip *big.Int, pct uint64) *big.Int {
	n := new(big.Int).Mul(tip, new(big.Int).SetUint64(pct))
	return n.Div(n, bigHundred)
}

func newPrice(tip, doubleBaseFee *big.Int) *price {
	return &price{
		GasTip: (*hexutil.Big)(tip),
		GasFee: (*hexutil.Big)(new(big.Int).Add(doubleBaseFee, tip)),
	}
}

// NewAcceptedTransactions creates a subscription that is notified each time a
// transaction is accepted by consensus (prior to execution). If fullTx is true
// the full tx is sent to the client, otherwise only the hash is sent.
func (c *customAPI) NewAcceptedTransactions(ctx context.Context, fullTx *bool) (*rpc.Subscription, error) {
	panic(errUnimplemented)
}
