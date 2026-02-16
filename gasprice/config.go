// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/avalanchego/vms/evm/acp176"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
)

const (
	// DefaultMaxCallBlockHistory is the number of blocks that can be fetched in
	// a single call to eth_feeHistory.
	DefaultMaxCallBlockHistory = 2048
	// DefaultMaxBlockHistory is the number of blocks from the last accepted
	// block that can be fetched in eth_feeHistory.
	//
	// DefaultMaxBlockHistory is chosen to be a value larger than the required
	// fee lookback window that MetaMask uses (20k blocks).
	DefaultMaxBlockHistory = 25_000
	// DefaultFeeHistoryCacheSize is chosen to be some value larger than
	// [DefaultMaxBlockHistory] to ensure all block lookups can be cached when
	// serving a fee history query.
	FeeHistoryCacheSize = 30_000
)

var (
	DefaultMaxPrice           = big.NewInt(150 * params.GWei)
	DefaultMinPrice           = big.NewInt(acp176.MinGasPrice)
	DefaultMaxLookbackSeconds = uint64(80)
)

type config struct {
	// BlocksCount specifies the number of recent blocks to fetch for gas price estimation.
	BlocksCount uint64
	// Percentile is a value between 0 and 100 that we use during gas price estimation to choose
	// the gas price estimate in which Percentile% of the gas estimate values in the array fall below it
	Percentile uint64
	// MaxLookbackSeconds specifies the maximum number of seconds that current timestamp
	// can differ from block timestamp in order to be included in gas price estimation
	MaxLookbackSeconds uint64
	// MaxCallBlockHistory specifies the maximum number of blocks that can be
	// fetched in a single eth_feeHistory call.
	MaxCallBlockHistory uint64
	// MaxBlockHistory specifies the furthest back behind the last accepted block that can
	// be requested by fee history.
	MaxBlockHistory uint64
	MaxPrice        *big.Int
	MinPrice        *big.Int
}

// Option configures estimator initialization.
type EstimatorOption = options.Option[config]

func defaultConfig() config {
	return config{
		BlocksCount:         1,
		MaxLookbackSeconds:  DefaultMaxLookbackSeconds,
		MaxCallBlockHistory: DefaultMaxCallBlockHistory,
		MaxBlockHistory:     DefaultMaxBlockHistory,
		MaxPrice:            DefaultMaxPrice,
		MinPrice:            DefaultMinPrice,
	}
}

// WithBlocks sets the number of blocks sampled for tip estimation.
func WithBlocks(blocks uint64) (EstimatorOption, error) {
	return options.Func[config](func(c *config) {
		c.BlocksCount = blocks
	}), nil
}

// WithPercentile sets the sampled percentile used for tip estimation.
func WithPercentile(percentile uint64) (EstimatorOption, error) {
	return options.Func[config](func(c *config) {
		c.Percentile = percentile
	}), nil
}

// WithMaxLookbackSeconds sets the timestamp-based lookback limit.
func WithMaxLookbackSeconds(maxLookbackSeconds uint64) (EstimatorOption, error) {
	if maxLookbackSeconds < 1 {
		return nil, fmt.Errorf("max lookback seconds (%d) is less than 1", maxLookbackSeconds)
	}
	return options.Func[config](func(c *config) {
		c.MaxLookbackSeconds = maxLookbackSeconds
	}), nil
}

// WithMaxCallBlockHistory sets the per-call block limit for fee history.
func WithMaxCallBlockHistory(maxCallBlockHistory uint64) (EstimatorOption, error) {
	if maxCallBlockHistory < 1 {
		return nil, fmt.Errorf("max call block history (%d) is less than 1", maxCallBlockHistory)
	}
	return options.Func[config](func(c *config) {
		c.MaxCallBlockHistory = maxCallBlockHistory
	}), nil
}

// WithMaxBlockHistory sets the maximum query depth for fee history.
func WithMaxBlockHistory(maxBlockHistory uint64) (EstimatorOption, error) {
	if maxBlockHistory < 1 {
		return nil, fmt.Errorf("max block history (%d) is less than 1", maxBlockHistory)
	}
	return options.Func[config](func(c *config) {
		c.MaxBlockHistory = maxBlockHistory
	}), nil
}

// WithMaxPrice sets the maximum suggested tip.
func WithMaxPrice(maxPrice *big.Int) (EstimatorOption, error) {
	if maxPrice == nil || maxPrice.Sign() <= 0 {
		return nil, fmt.Errorf("max price (%v) is nil or non-positive", maxPrice)
	}
	return options.Func[config](func(c *config) {
		c.MaxPrice = maxPrice
	}), nil
}

// WithMinPrice sets the minimum suggested tip.
func WithMinPrice(minPrice *big.Int) (EstimatorOption, error) {
	if minPrice == nil || minPrice.Sign() < 0 {
		return nil, fmt.Errorf("min price (%v) is nil or negative", minPrice)
	}
	return options.Func[config](func(c *config) {
		c.MinPrice = minPrice
	}), nil
}
