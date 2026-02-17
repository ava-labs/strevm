// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gasprice provides gas price estimation and fee history for the SAE.
package gasprice

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
)

const (
	// FeeHistoryCacheSize is chosen to be some value larger than
	// [DefaultMaxBlockHistory] to ensure all block lookups can be cached when
	// serving a fee history query.
	FeeHistoryCacheSize = 30_000
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
	// MaxPrice specifies the maximum suggested tip that's used to calculate the gas price estimate.
	MaxPrice *big.Int
	// MinPrice specifies the minimum suggested tip that's used to calculate the gas price estimate.
	MinPrice *big.Int
}

// EstimatorOption configures estimator initialization.
type EstimatorOption = options.Option[config]

func defaultConfig() config {
	return config{
		BlocksCount:         20,
		Percentile:          60,
		MaxLookbackSeconds:  uint64(80),
		MaxCallBlockHistory: 2048,
		// Default MaxBlockHistory is chosen to be a value larger than the required
		// fee lookback window that MetaMask uses (20k blocks).
		MaxBlockHistory: 25_000,
		MaxPrice:        big.NewInt(150 * params.GWei),
		MinPrice:        big.NewInt(1 * params.Wei),
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
	if percentile == 0 || percentile > 100 {
		return nil, fmt.Errorf("percentile (%d) is out of range (0, 100]", percentile)
	}
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
