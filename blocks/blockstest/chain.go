// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"slices"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/options"

	"github.com/ava-labs/strevm/blocks"
)

// A ChainBuilder builds a chain of blocks, maintaining necessary invariants.
type ChainBuilder struct {
	chain  []*blocks.Block
	byHash map[common.Hash]*blocks.Block

	opts []ChainOption
}

// NewChainBuilder returns a new ChainBuilder starting from the provided block,
// which MUST NOT be nil.
func NewChainBuilder(genesis *blocks.Block) *ChainBuilder {
	return &ChainBuilder{
		chain:  []*blocks.Block{genesis},
		byHash: make(map[common.Hash]*blocks.Block),
	}
}

// A ChainOption configures [ChainBuilder.NewBlock].
type ChainOption = options.Option[chainOptions]

// SetDefaultOptions sets the default options upon which all
// additional options passed to [ChainBuilder.NewBlock] are appended.
func (cb *ChainBuilder) SetDefaultOptions(opts ...ChainOption) {
	cb.opts = opts
}

type chainOptions struct {
	ethOpts []EthBlockOption
	saeOpts []BlockOption
}

// WithEthBlockOptions wraps the options that [ChainBuilder.NewBlock] propagates
// to [NewEthBlock].
func WithEthBlockOptions(opts ...EthBlockOption) ChainOption {
	return options.Func[chainOptions](func(co *chainOptions) {
		co.ethOpts = append(co.ethOpts, opts...)
	})
}

// WithBlockOptions wraps the options that [ChainBuilder.NewBlock] propagates to
// [NewBlock].
func WithBlockOptions(opts ...BlockOption) ChainOption {
	return options.Func[chainOptions](func(co *chainOptions) {
		co.saeOpts = append(co.saeOpts, opts...)
	})
}

func ethBlockOptions(opts []ChainOption) []EthBlockOption {
	return options.ApplyTo(&chainOptions{}, opts...).ethOpts
}

func blockOptions(opts []ChainOption) []BlockOption {
	return options.ApplyTo(&chainOptions{}, opts...).saeOpts
}

// NewBlock constructs and returns a new block in the chain.
func (cb *ChainBuilder) NewBlock(tb testing.TB, txs []*types.Transaction, opts ...ChainOption) *blocks.Block {
	tb.Helper()
	opts = slices.Concat(cb.opts, opts)

	last := cb.Last()
	eth := NewEthBlock(last.EthBlock(), txs, ethBlockOptions(opts)...)
	cb.chain = append(cb.chain, NewBlock(tb, eth, last, nil, blockOptions(opts)...)) // TODO(arr4n) support last-settled blocks

	return cb.Last()
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

// GetBlock returns the block with specified hash and height, and a flag
// indicating if it was found. If either argument does not match, it returns
// `nil, false`.
func (cb *ChainBuilder) GetBlock(h common.Hash, num uint64) (*blocks.Block, bool) {
	b, ok := cb.byHash[h]
	if !ok || b.NumberU64() != num {
		return nil, false
	}
	return b, true
}
