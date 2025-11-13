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

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saetest"
)

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
