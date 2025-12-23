// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blockstest

import (
	"math"
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

func OverrideBaseFee(tb testing.TB, b *blocks.Block, fee *uint256.Int) {
	tb.Helper()
	require.False(tb, b.Executed(), "overriding gas price of already-executed block")

	p := b.ParentBlock()
	require.True(tb, p.Executed(), "parent block must be executed to override gas price")

	clock, ok := withBaseFee(p.ExecutedByGasTime(), fee)
	if !ok {
		tb.Fatalf("Precise base fee of %v is impossible with gas target of %d", fee, p.ExecutedByGasTime().Target())
	}

	fakeP := NewBlock(tb, p.EthBlock(), p.ParentBlock(), p.LastSettled())
	err := fakeP.MarkExecuted(
		rawdb.NewMemoryDatabase(), // i.e. drop all database updates
		clock,
		p.ExecutedByWallTime(),
		p.BaseFee(),
		p.Receipts(),
		p.PostExecutionStateRoot(),
	)
	require.NoErrorf(tb, err, "%T.MarkExecuted() on faked parent", fakeP)

	fake := NewBlock(tb, b.EthBlock(), fakeP, b.LastSettled())
	require.NoError(tb, b.CopyAncestorsFrom(fake), "CopyAncestorsFrom([block with faked parent])")
}

func withBaseFee(start *gastime.Time, fee *uint256.Int) (*gastime.Time, bool) {
	unix := start.Unix()
	target := start.Target()

	minX := gas.Gas(0)
	maxX := gas.Gas(math.MaxUint64)

	var clock *gastime.Time
	for range 64 {
		x := minX + (maxX-minX)/2
		clock = gastime.New(unix, target, x)

		switch clock.BaseFee().Cmp(fee) {
		case -1:
			minX = x
		case 0:
			if x == 0 || gastime.New(0, target, x-1).BaseFee().Lt(fee) {
				return clock, true
			}
			fallthrough
		case 1:
			maxX = x
		}
	}
	return nil, false
}
