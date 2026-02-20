// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name: "valid default",
			cfg:  DefaultConfig(),
		},
		{
			name:    "percentile zero",
			cfg:     modifyDefaultConfig(func(c *Config) { c.Percentile = 0 }),
			wantErr: errPercentileOutOfRange,
		},
		{
			name:    "percentile above 100",
			cfg:     modifyDefaultConfig(func(c *Config) { c.Percentile = 101 }),
			wantErr: errPercentileOutOfRange,
		},
		{
			name: "percentile boundary 1",
			cfg:  modifyDefaultConfig(func(c *Config) { c.Percentile = 1 }),
		},
		{
			name: "percentile boundary 100",
			cfg:  modifyDefaultConfig(func(c *Config) { c.Percentile = 100 }),
		},
		{
			name:    "max lookback seconds zero",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxLookbackSeconds = 0 }),
			wantErr: errMaxLookbackSecondsZero,
		},
		{
			name:    "max call block history zero",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxCallBlockHistory = 0 }),
			wantErr: errMaxCallBlockHistoryZero,
		},
		{
			name:    "max block history zero",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxBlockHistory = 0 }),
			wantErr: errMaxBlockHistoryZero,
		},
		{
			name:    "max price nil",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxPrice = nil }),
			wantErr: errMaxPriceNil,
		},
		{
			name:    "max price zero",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxPrice = big.NewInt(0) }),
			wantErr: errMaxPriceNegative,
		},
		{
			name:    "max price negative",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MaxPrice = big.NewInt(-1) }),
			wantErr: errMaxPriceNegative,
		},
		{
			name:    "min price nil",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MinPrice = nil }),
			wantErr: errMinPriceNil,
		},
		{
			name:    "min price negative",
			cfg:     modifyDefaultConfig(func(c *Config) { c.MinPrice = big.NewInt(-1) }),
			wantErr: errMinPriceNegative,
		},
		{
			name: "min price zero is valid",
			cfg:  modifyDefaultConfig(func(c *Config) { c.MinPrice = big.NewInt(0) }),
		},
		{
			name: "max price less than min price",
			cfg: modifyDefaultConfig(func(c *Config) {
				c.MinPrice = big.NewInt(100)
				c.MaxPrice = big.NewInt(50)
			}),
			wantErr: errMaxPriceLessThanMinPrice,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func modifyDefaultConfig(modify func(*Config)) Config {
	cfg := DefaultConfig()
	modify(&cfg)
	return cfg
}
