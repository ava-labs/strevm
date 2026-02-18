// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gasprice provides gas price estimation and fee history for the SAE.
package gasprice

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/params"
)

const (
	// FeeHistoryCacheSize is chosen to be some value larger than
	// [DefaultMaxBlockHistory] to ensure all block lookups can be cached when
	// serving a fee history query.
	FeeHistoryCacheSize = 30_000
)

type Config struct {
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

func DefaultConfig() Config {
	return Config{
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

func (c *Config) Validate() error {
	if c.Percentile == 0 || c.Percentile > 100 {
		return fmt.Errorf("percentile (%d) is out of range (0, 100]", c.Percentile)
	}
	if c.MaxLookbackSeconds == 0 {
		return fmt.Errorf("max lookback seconds (%d) is 0", c.MaxLookbackSeconds)
	}
	if c.MaxCallBlockHistory == 0 {
		return fmt.Errorf("max call block history (%d) is 0", c.MaxCallBlockHistory)
	}
	if c.MaxBlockHistory == 0 {
		return fmt.Errorf("max block history (%d) is less than 1", c.MaxBlockHistory)
	}
	if c.MaxPrice == nil || c.MaxPrice.Sign() <= 0 {
		return fmt.Errorf("max price (%v) is nil or non-positive", c.MaxPrice)
	}
	if c.MinPrice == nil || c.MinPrice.Sign() < 0 {
		return fmt.Errorf("min price (%v) is nil or negative", c.MinPrice)
	}
	if c.MaxPrice.Cmp(c.MinPrice) < 0 {
		return fmt.Errorf("max price (%v) is less than min price (%v)", c.MaxPrice, c.MinPrice)
	}
	return nil
}
