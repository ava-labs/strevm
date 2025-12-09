// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		Target: newTarget,
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
	assert.Equal(t, expectedEndTime, tm.Unix(), "Unix time advanced by AfterBlock()")
	assert.Equal(t, newTarget, tm.Target(), "Target updated by AfterBlock()")
	// While the price technically could remain the same, being more strict
	// ensures the test is meaningful.
	assert.Greater(t, tm.Price(), enforcedPrice, "Price should not decrease in AfterBlock()")
}

func FuzzWorstCasePrice(f *testing.F) {
	f.Fuzz(func(
		t *testing.T,
		initTimestamp, initTarget, initExcess,
		time0, timeFrac0, used0, limit0, target0,
		time1, timeFrac1, used1, limit1, target1,
		time2, timeFrac2, used2, limit2, target2,
		time3, timeFrac3, used3, limit3, target3 uint64,
	) {
		initTarget = max(initTarget, 1)

		worstcase := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))
		actual := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))

		blocks := []struct {
			time     uint64
			timeFrac gas.Gas
			used     gas.Gas
			limit    gas.Gas
			target   gas.Gas
		}{
			{
				time:     time0,
				timeFrac: gas.Gas(timeFrac0),
				used:     gas.Gas(used0),
				limit:    gas.Gas(limit0),
				target:   gas.Gas(target0),
			},
			{
				time:     time1,
				timeFrac: gas.Gas(timeFrac1),
				used:     gas.Gas(used1),
				limit:    gas.Gas(limit1),
				target:   gas.Gas(target1),
			},
			{
				time:     time2,
				timeFrac: gas.Gas(timeFrac2),
				used:     gas.Gas(used2),
				limit:    gas.Gas(limit2),
				target:   gas.Gas(target2),
			},
			{
				time:     time3,
				timeFrac: gas.Gas(timeFrac3),
				used:     gas.Gas(used3),
				limit:    gas.Gas(limit3),
				target:   gas.Gas(target3),
			},
		}
		for _, block := range blocks {
			block.limit = max(block.used, block.limit)
			block.target = clampTarget(max(block.target, 1))
			block.timeFrac %= rateOf(block.target)

			header := &types.Header{
				Time: block.time,
			}
			hook := &hookstest.Stub{
				Target:        block.target,
				SubSecondTime: block.timeFrac,
			}

			worstcase.BeforeBlock(hook, header)
			actual.BeforeBlock(hook, header)

			require.LessOrEqualf(t, actual.Price(), worstcase.Price(), "actual <= worst-case %T.Price()", actual)

			// The crux of this test lies in the maintaining of this inequality
			// through the use of `limit` instead of `used`
			worstcase.AfterBlock(block.limit, hook, header)
			actual.AfterBlock(block.used, hook, header)
		}
	})
}
