// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"math"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/intmath"
)

// TestInvalidConfigRejected verifies that zero values for TargetToExcessScaling
// and MinPrice are rejected by AfterBlock.
func TestInvalidConfigRejected(t *testing.T) {
	const target = gas.Gas(1_000_000)

	tests := []struct {
		name     string
		config   hook.GasPriceConfig
		expected error
	}{
		{
			"zero_scaling",
			hook.GasPriceConfig{TargetToExcessScaling: 0, MinPrice: DefaultGasPriceConfig().MinPrice},
			errTargetToExcessScalingZero,
		},
		{
			"zero_min_price",
			hook.GasPriceConfig{TargetToExcessScaling: DefaultGasPriceConfig().TargetToExcessScaling, MinPrice: 0},
			errMinPriceZero,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := mustNew(t, time.Unix(42, 0), target, 0, DefaultGasPriceConfig())

			initialScaling := tm.config.targetToExcessScaling
			initialMinPrice := tm.config.minPrice
			hooks := &hookstest.Stub{
				Target:         target,
				GasPriceConfig: tt.config,
			}
			err := tm.AfterBlock(0, hooks, &types.Header{Time: 42})
			require.ErrorIs(t, err, tt.expected)

			// Config unchanged after rejected update
			assert.Equal(t, initialScaling, tm.config.targetToExcessScaling, "targetToExcessScaling changed")
			assert.Equal(t, initialMinPrice, tm.config.minPrice, "minPrice changed")
		})
	}
}

// TestTargetUpdateTiming verifies that the gas target is modified in AfterBlock
// rather than BeforeBlock.
func TestTargetUpdateTiming(t *testing.T) {
	const (
		initialTime           = 42
		initialTarget gas.Gas = 1_600_000
		initialExcess         = 1_234_567_890
	)
	tm := mustNew(t, time.Unix(initialTime, 0), initialTarget, initialExcess, DefaultGasPriceConfig())
	initialRate := tm.Rate()

	const (
		newTime   uint64 = initialTime + 1
		newTarget        = initialTarget + 100_000
	)
	hook := &hookstest.Stub{
		Target:         newTarget,
		GasPriceConfig: DefaultGasPriceConfig(),
	}
	header := &types.Header{
		Time: newTime,
	}

	initialPrice := tm.Price()
	tm.BeforeBlock(hook, header)
	assert.Equal(t, newTime, tm.Unix(), "Unix time advanced by BeforeBlock()")
	assert.Equal(t, initialTarget, tm.Target(), "Target not changed by BeforeBlock()")
	// While the price technically could remain the same, being more strict
	// ensures the test is meaningful.
	enforcedPrice := tm.Price()
	assert.Less(t, enforcedPrice, initialPrice, "Price should not increase in BeforeBlock()")
	if t.Failed() {
		t.FailNow()
	}

	const (
		secondsOfGasUsed = 3
		expectedEndTime  = newTime + secondsOfGasUsed
	)
	used := initialRate * secondsOfGasUsed
	require.NoError(t, tm.AfterBlock(used, hook, header), "AfterBlock()")
	assert.Equal(t, expectedEndTime, tm.Unix(), "Unix time advanced by AfterBlock() due to gas consumption")
	assert.Equal(t, newTarget, tm.Target(), "Target updated by AfterBlock()")
	// While the price technically could remain the same, being more strict
	// ensures the test is meaningful.
	assert.Greater(t, tm.Price(), enforcedPrice, "Price should not decrease in AfterBlock()")
}

func FuzzWorstCasePrice(f *testing.F) {
	f.Fuzz(func(
		t *testing.T,
		initTimestamp, initTarget, initExcess,
		time0, nanos0, used0, limit0, target0,
		time1, nanos1, used1, limit1, target1,
		time2, nanos2, used2, limit2, target2,
		time3, nanos3, used3, limit3, target3 uint64,
	) {
		initTarget = max(initTarget, 1)

		initUnix := int64(min(initTimestamp, math.MaxInt64)) //nolint:gosec // I can't believe I have to be explicit about this!
		worstcase := mustNew(t, time.Unix(initUnix, 0), gas.Gas(initTarget), gas.Gas(initExcess), DefaultGasPriceConfig())
		actual := mustNew(t, time.Unix(initUnix, 0), gas.Gas(initTarget), gas.Gas(initExcess), DefaultGasPriceConfig())

		blocks := []struct {
			time   uint64
			nanos  time.Duration
			used   gas.Gas
			limit  gas.Gas
			target gas.Gas
		}{
			{
				time:   time0,
				nanos:  time.Duration(nanos0 % 1e9), //nolint:gosec
				used:   gas.Gas(used0),
				limit:  gas.Gas(limit0),
				target: gas.Gas(target0),
			},
			{
				time:   time1,
				nanos:  time.Duration(nanos1 % 1e9), //nolint:gosec
				used:   gas.Gas(used1),
				limit:  gas.Gas(limit1),
				target: gas.Gas(target1),
			},
			{
				time:   time2,
				nanos:  time.Duration(nanos2 % 1e9), //nolint:gosec
				used:   gas.Gas(used2),
				limit:  gas.Gas(limit2),
				target: gas.Gas(target2),
			},
			{
				time:   time3,
				nanos:  time.Duration(nanos3 % 1e9), //nolint:gosec
				used:   gas.Gas(used3),
				limit:  gas.Gas(limit3),
				target: gas.Gas(target3),
			},
		}
		for _, block := range blocks {
			block.limit = max(block.used, block.limit)
			block.target = clampTarget(max(block.target, 1))

			header := &types.Header{
				Time: block.time,
			}
			hook := &hookstest.Stub{
				Target: block.target,
				Now: func() time.Time {
					return time.Unix(
						int64(block.time), //nolint:gosec // Won't overflow for a few millennia
						int64(block.nanos),
					)
				},
				GasPriceConfig: DefaultGasPriceConfig(),
			}

			worstcase.BeforeBlock(hook, header)
			actual.BeforeBlock(hook, header)

			// The crux of this test lies in the maintaining of this inequality
			// through the use of `limit` instead of `used` in `AfterBlock()`
			require.LessOrEqualf(t, actual.Price(), worstcase.Price(), "actual <= worst-case %T.Price()", actual)
			require.NoError(t, worstcase.AfterBlock(block.limit, hook, header), "worstcase.AfterBlock()")
			require.NoError(t, actual.AfterBlock(block.used, hook, header), "actual.AfterBlock()")
		}
	})
}

func absdiff(x uint64, y uint64) uint64 {
	if x < y {
		return y - x
	}
	return x - y
}

func FuzzPriceInvarianceAfterBlock(f *testing.F) {
	for _, s := range []struct {
		T, x, M, KonT         uint64
		newT, newM, newKonT   uint64
		initStatic, newStatic bool
	}{
		// Basic scaling change: K doubles, price should be maintained
		{
			T: 1e6, M: 1, KonT: 1, // i.e. K == 1e6
			x:    2e6,          // Initial price is M⋅exp(x/K) = exp(2/1) ~= 7
			newT: 1e6, newM: 1, // both unchanged
			newKonT: 2, // i.e. K == 2e6; without proper scaling, price becomes exp(2/2) ~= 2
		},
		// K at MaxUint64 boundary
		{
			T: 1e6, M: 1, KonT: math.MaxUint64,
			x:    2e6,
			newT: 1e6, newM: 1,
			newKonT: math.MaxUint64,
		},
		// MinPrice increase above current price: price should bump to new M
		{
			T: 1e6, M: 1, KonT: 87,
			x:       2e6, // price = 1 * e^(2/87) ~= 1.023
			newT:    1e6,
			newM:    100, // new M > current price, should bump
			newKonT: 87,
		},
		// MinPrice decrease: price should be maintained
		{
			T: 1e6, M: 100, KonT: 87,
			x:       1e6, // price > 100
			newT:    1e6,
			newM:    50, // M decreases
			newKonT: 87,
		},
		// MinPrice increase below current price: price should be maintained
		{
			T: 1e6, M: 1, KonT: 87,
			x:       100e6, // high excess = high price >> 10
			newT:    1e6,
			newM:    10, // new M < current price
			newKonT: 87,
		},
		// Large excess value
		{
			T: 1e6, M: 1, KonT: 87,
			x:       1e9, // very high excess
			newT:    1e6,
			newM:    1,
			newKonT: 100,
		},
		// High MinPrice with scaling change
		{
			T: 1e6, M: 1e9, KonT: 87,
			x:       5e6,
			newT:    1e6,
			newM:    1e9,
			newKonT: 50,
		},
		// Zero excess with config changes
		{
			T: 1e6, M: 1, KonT: 87,
			x:       0,
			newT:    1e6,
			newM:    1,
			newKonT: 50,
		},
		// Around MaxUint64 scaling with MinPrice decrease
		{
			T: 1e6, M: 26, KonT: math.MaxInt64 - 100,
			x:       1e6,
			newT:    1e6,
			newM:    1,
			newKonT: math.MaxInt64 - 10,
		},
		// MaxUint64 scaling with high excess min price at 11 gwei
		{
			T: 1e6, M: 1e12, KonT: 87,
			x:       1e9,
			newT:    1e6,
			newM:    1e12,
			newKonT: math.MaxUint64,
		},
		// Dynamic to static pricing: price should snap to newMinPrice
		{
			T: 1e6, M: 1, KonT: 87,
			x:         1e9, // high excess = high price
			newT:      1e6,
			newM:      1,
			newKonT:   87,
			newStatic: true,
		},
		// Static to dynamic pricing: price continuity from initMinPrice
		{
			T: 1e6, M: 100, KonT: 87,
			x:          5e6,
			initStatic: true,
			newT:       1e6,
			newM:       50, // M decreases, should maintain initMinPrice via excess
			newKonT:    87,
		},
		// Static to static with MinPrice change: price should be newMinPrice
		{
			T: 1e6, M: 100, KonT: 87,
			x:          5e6,
			initStatic: true,
			newT:       1e6,
			newM:       200,
			newKonT:    87,
			newStatic:  true,
		},
	} {
		f.Add(s.T, s.x, s.M, s.KonT, s.initStatic, s.newT, s.newM, s.newKonT, s.newStatic)
	}

	f.Fuzz(func(
		t *testing.T,
		initTarget, excess, initMinPrice, initScaling uint64, initStaticPricing bool,
		newTarget, newMinPrice, newScaling uint64, newStaticPricing bool,
	) {
		if initMinPrice == 0 || newMinPrice == 0 {
			t.Skip("Zero price coefficient")
		}
		if initScaling == 0 || newScaling == 0 {
			t.Skip("Zero scaling denominator")
		}

		tm := mustNew(t,
			time.Unix(0, 0),
			gas.Gas(initTarget),
			gas.Gas(excess),
			hook.GasPriceConfig{
				TargetToExcessScaling: gas.Gas(initScaling),
				MinPrice:              gas.Price(initMinPrice),
				StaticPricing:         initStaticPricing,
			},
		)
		initPrice := tm.Price()

		hooks := &hookstest.Stub{
			Target: gas.Gas(newTarget),
			GasPriceConfig: hook.GasPriceConfig{
				MinPrice:              gas.Price(newMinPrice),
				TargetToExcessScaling: gas.Gas(newScaling),
				StaticPricing:         newStaticPricing,
			},
		}

		// Consuming gas increases the excess, which changes the price. We're
		// only interested in invariance under changes in config.
		const gasUsed = 0
		require.NoError(t, tm.AfterBlock(gasUsed, hooks, nil), "AfterBlock()")

		want := initPrice
		if p := hooks.GasPriceConfig.MinPrice; newStaticPricing || p > initPrice {
			want = p
		} else {
			// When required excess for continuity exceeds the search cap, findExcessForPrice
			// returns that cap and the resulting price is M * e^(cap/K).
			// This means price continuity is not possible if required excess exceeds the search cap.
			newK := intmath.BoundedMultiply(gas.Gas(newScaling), gas.Gas(newTarget), math.MaxUint64)
			requiredApproxExcess := float64(newK) * math.Log(float64(initPrice)/float64(newMinPrice))
			if requiredApproxExcess > float64(math.MaxUint64) {
				want = gas.CalculatePrice(gas.Price(newMinPrice), math.MaxUint64, newK)
			}
		}

		got := tm.Price()

		// Due to integer arithmetic in binary search, exact price continuity isn't always
		// achievable. We allow a small tolerance: difference of at most 1 in the exponent
		// means the price can differ by at most a factor of e^(1/K), which for practical
		// K values is negligible. We use a simple absolute difference check for small
		// prices and relative check for larger ones.
		diff := absdiff(uint64(got), uint64(want))

		// Allow difference of 1 or 0.01% of the price, whichever is larger
		tolerance := max(uint64(1), uint64(want)/100_000)
		if diff > tolerance {
			t.Logf("Target: %d -> %d", initTarget, newTarget)
			t.Logf("Excess: %v (unchanged)", excess)
			t.Logf("Price: %d -> %d", initPrice, got)
			t.Logf("MinPrice: %d -> %d", initMinPrice, newMinPrice)
			t.Logf("TargetToExcessScaling: %d -> %d", initScaling, newScaling)
			t.Logf("StaticPricing: %v -> %v", initStaticPricing, newStaticPricing)
			t.Errorf("AfterBlock([0 gas consumed]) -> %T.Price() got %d want %d (diff %d > tolerance %d)", tm, got, want, diff, tolerance)
		}
	})
}
