// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// Avalanche-custom extensions to the eth namespace. These RPCs are not part of
// the standard Ethereum JSON-RPC spec or geth, but are exposed by Avalanche
// nodes for compatibility with tooling that depends on them (e.g. Core).
//
// Reference implementations live in graft/coreth/internal/ethapi/api_extra.go
// and graft/coreth/internal/ethapi/api.coreth.go.

import (
	"context"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
)

// ethExtAPI exposes Avalanche-custom extensions under the "eth" namespace:
//   - eth_getChainConfig
//   - eth_baseFee
//   - eth_suggestPriceOptions
//   - eth_callDetailed
//   - eth_newAcceptedTransactions (subscription)
type ethExtAPI struct {
	b *ethAPIBackend
}

// GetChainConfig returns the chain configuration.
func (api *ethExtAPI) GetChainConfig(ctx context.Context) *params.ChainConfig {
	panic(errUnimplemented)
}

// BaseFee returns the base fee of the last executed block.
func (api *ethExtAPI) BaseFee(ctx context.Context) (*hexutil.Big, error) {
	panic(errUnimplemented)
}

// DetailedExecutionResult is the response for eth_callDetailed.
type DetailedExecutionResult struct {
	UsedGas    uint64        `json:"gas"`
	ErrCode    int           `json:"errCode"`
	Err        string        `json:"err"`
	ReturnData hexutil.Bytes `json:"returnData"`
}

// CallDetailed performs the same call as eth_call, but returns gas usage and
// error details instead of just the return data.
func (api *ethExtAPI) CallDetailed(ctx context.Context, args any, blockNrOrHash rpc.BlockNumberOrHash, overrides any) (*DetailedExecutionResult, error) {
	panic(errUnimplemented)
}

// Price represents a single gas-price suggestion.
type Price struct {
	GasTip *hexutil.Big `json:"maxPriorityFeePerGas"`
	GasFee *hexutil.Big `json:"maxFeePerGas"`
}

// PriceOptions groups slow/normal/fast gas-price suggestions.
type PriceOptions struct {
	Slow   *Price `json:"slow"`
	Normal *Price `json:"normal"`
	Fast   *Price `json:"fast"`
}

// SuggestPriceOptions returns gas-price suggestions at three speed tiers.
func (api *ethExtAPI) SuggestPriceOptions(ctx context.Context) (*PriceOptions, error) {
	panic(errUnimplemented)
}

// NewAcceptedTransactions creates a subscription that is triggered each time a
// transaction is accepted into consensus (prior to execution). If fullTx is
// true the full tx is sent to the client, otherwise only the hash is sent.
func (api *ethExtAPI) NewAcceptedTransactions(ctx context.Context, fullTx *bool) (*rpc.Subscription, error) {
	panic(errUnimplemented)
}
