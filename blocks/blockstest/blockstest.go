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
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
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

// NewGenesis is a convenience wrapper around [saetest.Genesis] and [NewBlock],
// also marking the returned block as both executed and synchronous.
func NewGenesis(tb testing.TB, db ethdb.Database, config *params.ChainConfig, alloc types.GenesisAlloc) *blocks.Block {
	tb.Helper()
	b := NewBlock(tb, saetest.Genesis(tb, db, config, alloc), nil, nil)
	require.NoErrorf(tb, b.MarkExecuted(db, gastime.New(0, 1, 0), time.Time{}, new(big.Int), nil, b.SettledStateRoot()), "%T.MarkExecuted()", b)
	require.NoErrorf(tb, b.MarkSynchronous(), "%T.MarkSynchronous()", b)
	return b
}

// A ChainBuilder builds a chain of blocks, maintaining necessary invariants.
type ChainBuilder struct {
	chain  []*blocks.Block
	byHash map[common.Hash]*blocks.Block
}

// NewChainBuilder returns a new ChainBuilder starting from the provided block,
// which MUST NOT be nil.
func NewChainBuilder(genesis *blocks.Block) *ChainBuilder {
	return &ChainBuilder{
		chain: []*blocks.Block{genesis},
		byHash: map[common.Hash]*blocks.Block{
			genesis.Hash(): genesis,
		},
	}
}

// NewBlock constructs and returns a new block in the chain.
func (cb *ChainBuilder) NewBlock(tb testing.TB, txs ...*types.Transaction) *blocks.Block {
	last := cb.Last()
	hdr := &types.Header{
		Number:     new(big.Int).Add(last.Number(), big.NewInt(1)),
		ParentHash: last.Hash(),
		BaseFee:    big.NewInt(1),
	}
	eth := types.NewBlock(hdr, txs, nil, nil, saetest.TrieHasher())

	cb.chain = append(cb.chain, NewBlock(tb, eth, last, nil))
	return cb.Last()
}

// Last returns the last block to be built by the builder, which MAY be the
// genesis block passed to the constructor.
func (cb *ChainBuilder) Last() *blocks.Block {
	return cb.chain[len(cb.chain)-1]
}

// GetBlock returns the block with the given hash and number. If either argument
// fails to match a known block then GetBlock returns `nil, false`.
func (cb *ChainBuilder) GetBlock(hash common.Hash, number uint64) (*blocks.Block, bool) {
	b, ok := cb.byHash[hash]
	if !ok || b == nil || b.NumberU64() != number {
		return nil, false
	}
	return b, true
}
