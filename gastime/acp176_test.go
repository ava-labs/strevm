// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/hook/hookstest"
)

// TestTargetUpdateTiming verifies that the gas target is modified in AfterBlock
// rather than BeforeBlock.
func TestTargetUpdateTiming(t *testing.T) {
	const (
		initialTime           = 42
		initialTarget gas.Gas = 1_600_000
		initialExcess         = 1_234_567_890
	)
	tm := New(initialTime, initialTarget, initialExcess)
	initialRate := tm.Rate()

	const (
		newTime   uint64 = initialTime + 1
		newTarget        = initialTarget + 100_000
	)
	hook := &hookstest.Stub{
		GasConfig: hook.GasConfig{
			Target: newTarget,
		},
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

func FuzzPriceInvarianceAfterBlock(f *testing.F) {
	for _, s := range []struct {
		T, x, M, KonT       uint64
		newT, newM, newKonT uint64
	}{
		{
			T: 1e6, M: 1, KonT: 1, // i.e. K == 1e6
			x:    2e6,          // Initial price is M⋅exp(x/K) = exp(2/1) ~= 7
			newT: 1e6, newM: 1, // both unchanged
			newKonT: 2, // i.e. K == 2e6; without proper scaling, price becomes exp(2/2) ~= 2
		},
	} {
		f.Add(s.T, s.x, s.M, s.KonT, s.newT, s.newM, s.newKonT)
	}

	f.Fuzz(func(
		t *testing.T,
		initTarget, excess, initMinPrice, initScaling uint64,
		newTarget, newMinPrice, newScaling uint64,
	) {
		if initMinPrice == 0 || newMinPrice == 0 {
			t.Skip("Zero price coefficient")
		}
		if initScaling == 0 || newScaling == 0 {
			t.Skip("Zero scaling denominator")
		}

		tm := New(
			0,
			gas.Gas(initTarget),
			gas.Gas(excess),
			WithMinPrice(gas.Price(initMinPrice)),
			WithTargetToExcessScaling(gas.Gas(initScaling)),
		)
		initPrice := tm.Price()

		hooks := &hookstest.Stub{
			GasConfig: hook.GasConfig{
				Target:                gas.Gas(newTarget),
				MinPrice:              gas.Price(newMinPrice),
				TargetToExcessScaling: gas.Gas(newScaling),
			},
		}

		// Consuming gas increases the excess, which changes the price. We're
		// only interested in invariance under changes in config.
		const gasUsed = 0
		require.NoError(t, tm.AfterBlock(gasUsed, hooks, nil), "AfterBlock()")

		want := initPrice
		if p := hooks.GasConfig.MinPrice; p > initPrice {
			want = p
		}
		if got := tm.Price(); got != want {
			t.Logf("Target: %d -> %d", initTarget, newTarget)
			t.Logf("Excess: %v (unchanged)", excess)
			t.Logf("MinPrice: %d -> %d", initMinPrice, newMinPrice)
			t.Logf("TargetToExcessScaling: %d -> %d", initScaling, newScaling)
			t.Errorf("AfterBlock([0 gas consumed]) -> %T.Price() got %d want %d (i.e. unchanged)", tm, got, want)
		}
	})
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

		worstcase := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))
		actual := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))

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
				GasConfig: hook.GasConfig{
					Target: block.target,
				},
				Now: func() time.Time {
					return time.Unix(
						int64(block.time), //nolint:gosec // Won't overflow for a few millennia
						int64(block.nanos),
					)
				},
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
