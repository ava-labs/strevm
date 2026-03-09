// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math/big"

	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"

	_ "github.com/ava-labs/libevm/event"
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
// It returns nil if the estimate is unavailable.
func (c *customAPI) BaseFee(ctx context.Context) *hexutil.Big {
	return (*hexutil.Big)(c.estimateNextBaseFee())
}

// estimateNextBaseFee returns the worst-case upper bound on the next block's
// base fee. It returns nil when the last accepted block has no worst-case
// bounds, which happens on startup.
func (c *customAPI) estimateNextBaseFee() *big.Int {
	bounds := c.b.vm.last.accepted.Load().WorstCaseBounds()
	if bounds == nil {
		return nil
	}
	return bounds.LatestEndTime.BaseFee().ToBig()
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
func (c *customAPI) CallDetailed(ctx context.Context, args ethapi.TransactionArgs, blockNrOrHash rpc.BlockNumberOrHash, overrides *ethapi.StateOverride) (*detailedExecutionResult, error) {
	result, err := ethapi.DoCall(ctx, c.b, args, blockNrOrHash, overrides, nil, c.b.RPCEVMTimeout(), c.b.RPCGasCap())
	if err != nil {
		return nil, err
	}
	reply := &detailedExecutionResult{
		UsedGas:    result.UsedGas,
		ReturnData: result.ReturnData,
	}
	if result.Err != nil {
		if rpcErr, ok := result.Err.(rpc.Error); ok {
			reply.ErrCode = rpcErr.ErrorCode()
		}
		reply.Err = result.Err.Error()
	}
	if revert := result.Revert(); len(revert) > 0 {
		revErr := ethapi.NewRevertError(revert)
		reply.ErrCode = revErr.ErrorCode()
		reply.Err = revErr.Error()
	}
	return reply, nil
}

// price represents a single gas-price suggestion.
type price struct {
	GasTip *hexutil.Big `json:"maxPriorityFeePerGas"`
	GasFee *hexutil.Big `json:"maxFeePerGas"`
}

func newPrice(tip, baseFee *big.Int) *price {
	return &price{
		GasTip: (*hexutil.Big)(tip),
		GasFee: (*hexutil.Big)(new(big.Int).Add(tip, baseFee)),
	}
}

// priceOptions groups slow/normal/fast gas-price suggestions.
type priceOptions struct {
	Slow   *price `json:"slow"`
	Normal *price `json:"normal"`
	Fast   *price `json:"fast"`
}

var minGasTip = big.NewInt(params.Wei)

func newPriceOptions(tip, baseFee *big.Int) *priceOptions {
	const (
		slowTipPercent = 95
		fastTipPercent = 105
	)
	slowTip := math.BigMax(scale(tip, slowTipPercent), minGasTip)
	fastTip := scale(tip, fastTipPercent)
	return &priceOptions{
		Slow:   newPrice(slowTip, baseFee),
		Normal: newPrice(tip, baseFee),
		Fast:   newPrice(fastTip, baseFee),
	}
}

var big100 = big.NewInt(100)

// scale returns v * percent / 100.
func scale(v *big.Int, percent uint64) *big.Int {
	x := new(big.Int).SetUint64(percent)
	x.Mul(x, v)
	return x.Div(x, big100)
}

// SuggestPriceOptions returns gas-price suggestions at three speed tiers.
func (c *customAPI) SuggestPriceOptions(ctx context.Context) (*priceOptions, error) {
	tip, err := c.b.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	doubleBaseFee := c.estimateNextBaseFee()
	if doubleBaseFee == nil {
		return nil, nil
	}
	doubleBaseFee.Lsh(doubleBaseFee, 1)
	return newPriceOptions(tip, doubleBaseFee), nil
}

// NewAcceptedTransactions creates a subscription that is notified each time a
// transaction is accepted by consensus (prior to execution). If fullTx is true
// the full tx is sent to the client, otherwise only the hash is sent.
func (c *customAPI) NewAcceptedTransactions(ctx context.Context, fullTx *bool) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}
	rpcSub := notifier.CreateSubscription()
	// [event.FeedOf.Send] blocks until all subscribers receive the value, so a
	// slow reader can stall the sender. An ample buffer avoids this in practice.
	// See https://github.com/ava-labs/libevm/blob/3d7f8934ee6c/event/feedof.go#L49-L50
	const acceptedBlocksBuf = 128
	go func() {
		ch := make(chan *blocks.Block, acceptedBlocksBuf)
		sub := c.b.vm.acceptedBlocks.Subscribe(ch)
		chainConfig := c.b.ChainConfig()
		for {
			select {
			case block := <-ch:
				hash := block.Hash()
				num := block.NumberU64()
				buildTime := block.BuildTime()
				baseFee := block.Header().BaseFee
				for i, tx := range block.Transactions() {
					if fullTx != nil && *fullTx {
						rpcTx := ethapi.NewRPCTransaction(tx, hash, num, buildTime, uint64(i), baseFee, chainConfig) //nolint:gosec // i will never overflow uint64
						_ = notifier.Notify(rpcSub.ID, rpcTx)
					} else {
						_ = notifier.Notify(rpcSub.ID, tx.Hash())
					}
				}
			case <-rpcSub.Err():
				sub.Unsubscribe()
				return
			case <-notifier.Closed():
				sub.Unsubscribe()
				return
			}
		}
	}()
	return rpcSub, nil
}
