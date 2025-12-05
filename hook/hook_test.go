// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package hook_test

import (
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/ava-labs/strevm/hook"
)

// TestTargetUpdateTiming verifies that the gas target is modified in AfterBlock
// rather than BeforeBlock.
func TestTargetUpdateTiming(t *testing.T) {
	const (
		initialTime           = 42
		initialTarget gas.Gas = 1_600_000
		initialExcess         = 1_234_567_890
	)
	tm := gastime.New(initialTime, initialTarget, initialExcess)

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
	block := types.NewBlock(header, nil, nil, nil, saetest.TrieHasher())

	initialPrice := tm.Price()
	require.NoError(t, BeforeBlock(hook, params.TestRules, nil, block, tm), "BeforeBlock()")
	assert.Equal(t, newTime, tm.Unix(), "Unix time advanced by BeforeBlock()")
	assert.Equal(t, initialTarget, tm.Target(), "Target not changed by BeforeBlock()")

	enforcedPrice := tm.Price()
	assert.LessOrEqual(t, enforcedPrice, initialPrice, "Price should not increase in BeforeBlock()")
	if t.Failed() {
		t.FailNow()
	}

	const (
		secondsOfGasUsed         = 3
		initialRate              = initialTarget * gastime.TargetToRate
		used             gas.Gas = initialRate * secondsOfGasUsed
		expectedEndTime          = newTime + secondsOfGasUsed
	)
	require.NoError(t, AfterBlock(hook, nil, block, tm, used, nil), "AfterBlock()")
	assert.Equal(t, expectedEndTime, tm.Unix(), "Unix time advanced by AfterBlock()")
	assert.Equal(t, newTarget, tm.Target(), "Target updated by AfterBlock()")
	assert.GreaterOrEqual(t, tm.Price(), enforcedPrice, "Price should not decrease in AfterBlock()")
}
