// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gasprice provides gas price estimation and fee history for the SAE.
package gasprice

import (
	"errors"
	"math/big"

	"github.com/ava-labs/libevm/params"
)

const (
	// FeeHistoryCacheSize is chosen to be some value larger than
	// [DefaultMaxBlockHistory] to ensure all block lookups can be cached when
	// serving a fee history query.
	FeeHistoryCacheSize = 30_000
)

var (
	errPercentileOutOfRange     = errors.New("percentile is out of range (0, 100]")
	errMaxLookbackSecondsZero   = errors.New("max lookback seconds is 0")
	errMaxCallBlockHistoryZero  = errors.New("max call block history is 0")
	errMaxBlockHistoryZero      = errors.New("max block history is 0")
	errMaxPriceNil              = errors.New("max price is nil")
	errMaxPriceNegative         = errors.New("max price is negative")
	errMinPriceNil              = errors.New("min price is nil")
	errMinPriceNegative         = errors.New("min price is negative")
	errMaxPriceLessThanMinPrice = errors.New("max price is less than min price")
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
		return errPercentileOutOfRange
	}
	if c.MaxLookbackSeconds == 0 {
		return errMaxLookbackSecondsZero
	}
	if c.MaxCallBlockHistory == 0 {
		return errMaxCallBlockHistoryZero
	}
	if c.MaxBlockHistory == 0 {
		return errMaxBlockHistoryZero
	}
	if c.MaxPrice == nil {
		return errMaxPriceNil
	}
	if c.MaxPrice.Sign() <= 0 {
		return errMaxPriceNegative
	}
	if c.MinPrice == nil {
		return errMinPriceNil
	}
	if c.MinPrice.Sign() < 0 {
		return errMinPriceNegative
	}
	if c.MaxPrice.Cmp(c.MinPrice) < 0 {
		return errMaxPriceLessThanMinPrice
	}
	return nil
}
