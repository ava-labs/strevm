// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"slices"
	"sync"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/options"

	"github.com/ava-labs/strevm/blocks"
)

// A ChainBuilder builds a chain of blocks, maintaining necessary invariants.
type ChainBuilder struct {
	chain        []*blocks.Block
	blocksByHash sync.Map

	defaultOpts []ChainOption
}

// NewChainBuilder returns a new ChainBuilder starting from the provided block,
// which MUST NOT be nil.
func NewChainBuilder(genesis *blocks.Block, defaultOpts ...ChainOption) *ChainBuilder {
	c := &ChainBuilder{
		chain: []*blocks.Block{genesis},
	}
	c.SetDefaultOptions(defaultOpts...)
	c.blocksByHash.Store(genesis.Hash(), genesis)
	return c
}

// A ChainOption configures [ChainBuilder.NewBlock].
type ChainOption = options.Option[chainOptions]

// SetDefaultOptions sets the default options upon which all
// additional options passed to [ChainBuilder.NewBlock] are appended.
func (cb *ChainBuilder) SetDefaultOptions(opts ...ChainOption) {
	cb.defaultOpts = opts
}

type chainOptions struct {
	eth []EthBlockOption
	sae []BlockOption
}

// WithEthBlockOptions wraps the options that [ChainBuilder.NewBlock] propagates
// to [NewEthBlock].
func WithEthBlockOptions(opts ...EthBlockOption) ChainOption {
	return options.Func[chainOptions](func(co *chainOptions) {
		co.eth = append(co.eth, opts...)
	})
}

// WithBlockOptions wraps the options that [ChainBuilder.NewBlock] propagates to
// [NewBlock].
func WithBlockOptions(opts ...BlockOption) ChainOption {
	return options.Func[chainOptions](func(co *chainOptions) {
		co.sae = append(co.sae, opts...)
	})
}

// NewBlock constructs and returns a new block in the chain.
func (cb *ChainBuilder) NewBlock(tb testing.TB, txs []*types.Transaction, opts ...ChainOption) *blocks.Block {
	tb.Helper()

	allOpts := new(chainOptions)
	options.ApplyTo(allOpts, cb.defaultOpts...)
	options.ApplyTo(allOpts, opts...)

	last := cb.Last()
	eth := NewEthBlock(last.EthBlock(), txs, allOpts.eth...)
	b := NewBlock(tb, eth, last, nil, allOpts.sae...) // TODO(arr4n) support last-settled blocks
	cb.chain = append(cb.chain, b)
	cb.blocksByHash.Store(b.Hash(), b)

	return b
}

// Insert adds a block to the chain.
func (cb *ChainBuilder) Insert(block *blocks.Block) {
	cb.chain = append(cb.chain, block)
	cb.blocksByHash.Store(block.Hash(), block)
}

// Last returns the last block to be built by the builder, which MAY be the
// genesis block passed to the constructor.
func (cb *ChainBuilder) Last() *blocks.Block {
	return cb.chain[len(cb.chain)-1]
}

// AllBlocks returns all blocks, including the genesis passed to
// [NewChainBuilder].
func (cb *ChainBuilder) AllBlocks() []*blocks.Block {
	return slices.Clone(cb.chain)
}

// AllExceptGenesis returns all blocks created with [ChainBuilder.NewBlock].
func (cb *ChainBuilder) AllExceptGenesis() []*blocks.Block {
	return slices.Clone(cb.chain[1:])
}

var _ blocks.Source = (*ChainBuilder)(nil).GetBlock

// GetBlock returns the block with specified hash and height, and a flag
// indicating if it was found. If either argument does not match, it returns
// `nil, false`.
func (cb *ChainBuilder) GetBlock(h common.Hash, num uint64) (*blocks.Block, bool) {
	ifc, _ := cb.blocksByHash.Load(h)
	b, ok := ifc.(*blocks.Block)
	if !ok || b.NumberU64() != num {
		return nil, false
	}
	return b, true
}

// GetHashAtHeight returns the hash of the block at the given height, and a flag indicating if it was found.
// If the height is greater than the number of blocks in the chain, it returns an empty hash and false.
func (cb *ChainBuilder) GetHashAtHeight(num uint64) (common.Hash, bool) {
	if num >= uint64(len(cb.chain)) {
		return common.Hash{}, false
	}
	block := cb.chain[num]
	return block.Hash(), block != nil && block.NumberU64() == num
}

// GetNumberByHash returns the number of the block with the given hash, and a flag indicating if it was found.
func (cb *ChainBuilder) GetNumberByHash(h common.Hash) (uint64, bool) {
	ifc, _ := cb.blocksByHash.Load(h)
	b, ok := ifc.(*blocks.Block)
	if !ok {
		return 0, false
	}
	return b.NumberU64(), true
}
