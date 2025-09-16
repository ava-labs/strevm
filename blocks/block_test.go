// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEthBlock(num, time uint64, parent *types.Block) *types.Block {
	hdr := &types.Header{
		Number: new(big.Int).SetUint64(num),
		Time:   time,
	}
	if parent != nil {
		hdr.ParentHash = parent.Hash()
	}
	return types.NewBlockWithHeader(hdr)
}

func newBlock(tb testing.TB, eth *types.Block, parent, lastSettled *Block) *Block {
	tb.Helper()
	b, err := New(eth, parent, lastSettled, logging.NoLog{})
	require.NoError(tb, err, "New()")
	return b
}

func newChain(tb testing.TB, startHeight, total uint64, lastSettledAtHeight map[uint64]uint64) []*Block {
	tb.Helper()

	var (
		ethParent *types.Block
		parent    *Block
		blocks    []*Block
	)
	byNum := make(map[uint64]*Block)

	if lastSettledAtHeight == nil {
		lastSettledAtHeight = make(map[uint64]uint64)
	}

	for i := range total {
		n := startHeight + i

		var settle *Block
		if s, ok := lastSettledAtHeight[n]; ok {
			settle = byNum[s]
		}

		byNum[n] = newBlock(tb, newEthBlock(n, n /*time*/, ethParent), parent, settle)
		blocks = append(blocks, byNum[n])

		parent = byNum[n]
		ethParent = parent.EthBlock()
	}

	return blocks
}

func TestSetAncestors(t *testing.T) {
	parent := newBlock(t, newEthBlock(5, 5, nil), nil, nil)
	lastSettled := newBlock(t, newEthBlock(3, 0, nil), nil, nil)
	child := newEthBlock(6, 6, parent.EthBlock())

	t.Run("incorrect_parent", func(t *testing.T) {
		// Note that the arguments to [New] are inverted.
		_, err := New(child, lastSettled, parent, logging.NoLog{})
		require.ErrorIs(t, err, errParentHashMismatch, "New() with inverted parent and last-settled blocks")
	})

	source := newBlock(t, child, parent, lastSettled)
	dest := newBlock(t, child, nil, nil)

	t.Run("destination_before_copy", func(t *testing.T) {
		assert.Nilf(t, dest.ParentBlock(), "%T.ParentBlock()", dest)
		assert.Nilf(t, dest.LastSettled(), "%T.LastSettled()", dest)
	})
	if t.Failed() {
		t.FailNow()
	}

	require.NoError(t, dest.CopyAncestorsFrom(source), "CopyAncestorsFrom()")
	if diff := cmp.Diff(source, dest, CmpOpt()); diff != "" {
		t.Errorf("After %T.CopyAncestorsFrom(); diff (-want +got):\n%s", dest, diff)
	}

	t.Run("incompatible_destination_block", func(t *testing.T) {
		ethB := newEthBlock(dest.Height()+1 /*mismatch*/, dest.BuildTime(), parent.EthBlock())
		dest := newBlock(t, ethB, nil, nil)
		require.ErrorIs(t, dest.CopyAncestorsFrom(source), errHashMismatch)
	})
}
