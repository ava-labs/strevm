// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saetest"
)

// NewEthBlock constructs a raw Ethereum block with the given arguments.
func NewEthBlock(parent *types.Block, time uint64, txs types.Transactions) *types.Block {
	hdr := &types.Header{
		Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
		Time:       time,
		ParentHash: parent.Hash(),
	}
	return types.NewBlock(hdr, txs, nil, nil, saetest.TrieHasher())
}

// NewBlock constructs an SAE block, wrapping the raw Ethereum block.
func NewBlock(tb testing.TB, eth *types.Block, parent, lastSettled *blocks.Block) *blocks.Block {
	tb.Helper()
	b, err := blocks.New(eth, parent, lastSettled, saetest.NewTBLogger(tb, logging.Warn))
	require.NoError(tb, err, "New()")
	return b
}
