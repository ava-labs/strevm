// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"testing"

	"github.com/ava-labs/libevm/core/types"

	"github.com/ava-labs/strevm/blocks"
)

// A ChainBuilder builds a chain of blocks, maintaining necessary invariants.
type ChainBuilder struct {
	chain []*blocks.Block
}

// NewChainBuilder returns a new ChainBuilder starting from the provided block,
// which MUST NOT be nil.
func NewChainBuilder(genesis *blocks.Block) *ChainBuilder {
	return &ChainBuilder{
		chain: []*blocks.Block{genesis},
	}
}

// NewBlock constructs and returns a new block in the chain.
func (cb *ChainBuilder) NewBlock(tb testing.TB, txs []*types.Transaction, opts ...EthBlockOption) *blocks.Block {
	tb.Helper()
	last := cb.Last()
	eth := NewEthBlock(last.EthBlock(), txs, opts...)
	cb.chain = append(cb.chain, NewBlock(tb, eth, last, nil)) // TODO(arr4n) support last-settled blocks
	return cb.Last()
}

// Last returns the last block to be built by the builder, which MAY be the
// genesis block passed to the constructor.
func (cb *ChainBuilder) Last() *blocks.Block {
	return cb.chain[len(cb.chain)-1]
}
