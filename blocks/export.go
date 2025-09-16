// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
)

// EthBlock returns the raw EVM block wrapped by b. Prefer accessing its
// properties via the methods aliased on [Block] as some (e.g.
// [types.Block.Root]) have ambiguous interpretation under SAE.
func (b *Block) EthBlock() *types.Block { return b.b }

// SettledStateRoot returns the state root after execution of the last block
// settled by b. It is a convenience wrapper for calling [types.Block.Root] on
// the embedded [types.Block].
func (b *Block) SettledStateRoot() common.Hash {
	return b.b.Root()
}

// Hash returns [types.Block.Hash] from the wrapped [types.Block].
func (b *Block) Hash() common.Hash { return b.b.Hash() }

// ParentHash returns [types.Block.ParentHash] from the wrapped [types.Block].
func (b *Block) ParentHash() common.Hash { return b.b.ParentHash() }

// NumberU64 returns [types.Block.NumberU64] from the wrapped [types.Block].
func (b *Block) NumberU64() uint64 { return b.b.NumberU64() }

// Time returns [types.Block.Time] from the wrapped [types.Block].
func (b *Block) Time() uint64 { return b.b.Time() }

// Number returns [types.Block.Number] from the wrapped [types.Block].
func (b *Block) Number() *big.Int { return b.b.Number() }
