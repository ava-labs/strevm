// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blocks defines [Streaming Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blocks

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"go.uber.org/zap"
)

// A Block extends a [types.Block] to track SAE-defined concepts of async
// execution and settlement. It MUST be constructed with [New].
type Block struct {
	b *types.Block
	// Invariant: ancestry is non-nil and contains non-nil pointers i.f.f. the
	// block hasn't itself been settled. A synchronous block (e.g. SAE genesis
	// or the last pre-SAE block) is always considered settled. See [New] for
	// caveats during construction.
	//
	// Rationale: the ancestral pointers form a linked list that would prevent
	// garbage collection if not severed. Once a block is settled there is no
	// need to inspect its history so we sacrifice the ancestors to the GC
	// Overlord as a sign of our unwavering fealty. See [InMemoryBlockCount] for
	// observability.
	ancestry atomic.Pointer[ancestry]
	// Only the genesis block or the last pre-SAE block is synchronous. These
	// are self-settling by definition so their `ancestry` MUST be nil.
	synchronous bool
	// Non-nil i.f.f. [Block.MarkExecuted] has returned without error.
	execution atomic.Pointer[executionResults]

	// Allows this block to be ruled out as able to be settled at a particular
	// time (i.e. if this field is >= said time). The pointer MAY be nil if
	// execution is yet to commence. For more details, see
	// [Block.SetInterimExecutionTime for setting and [LastToSettleAt] for
	// usage.
	executionExceededSecond atomic.Pointer[uint64]

	executed chan struct{} // closed after `execution` is set
	settled  chan struct{} // closed after `ancestry` is cleared

	log logging.Logger
}

var inMemoryBlockCount atomic.Int64

// InMemoryBlockCount returns the number of blocks created with [New] that are
// yet to have their GC finalizers run.
func InMemoryBlockCount() int64 {
	return inMemoryBlockCount.Load()
}

// New constructs a new Block.
//
// While both the `parent` and `lastSettled` arguments MAY be nil, this will
// result in an invalid Block as it breaks important invariants. In such
// situations, [Block.CopyAncestorsFrom] MUST then be called before further use
// of the Block. In practice, this SHOULD only be done when parsing an encoded
// Block.
func New(eth *types.Block, parent, lastSettled *Block, log logging.Logger) (*Block, error) {
	b := &Block{
		b:        eth,
		executed: make(chan struct{}),
		settled:  make(chan struct{}),
	}

	inMemoryBlockCount.Add(1)
	runtime.AddCleanup(b, func(struct{}) {
		inMemoryBlockCount.Add(-1)
	}, struct{}{})

	if err := b.setAncestors(parent, lastSettled); err != nil {
		return nil, err
	}
	b.log = log.With(
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)
	return b, nil
}

var (
	errParentHashMismatch         = errors.New("block-parent hash mismatch")
	errBlockHeightNotIncrementing = errors.New("block height not incrementing")
	errHashMismatch               = errors.New("block hash mismatch")
)

func (b *Block) setAncestors(parent, lastSettled *Block) error {
	if parent != nil {
		if got, want := parent.Hash(), b.ParentHash(); got != want {
			return fmt.Errorf("%w: constructing Block with parent hash %v; expecting %v", errParentHashMismatch, got, want)
		}
		if got, want := parent.Number(), new(big.Int).Sub(b.Number(), big.NewInt(1)); got.Cmp(want) != 0 {
			return fmt.Errorf("%w: constructing Block with parent height %v and own height %v", errBlockHeightNotIncrementing, parent.Number(), b.Number())
		}
	}
	b.ancestry.Store(&ancestry{
		parent:      parent,
		lastSettled: lastSettled,
	})
	return nil
}

// CopyAncestorsFrom populates the [Block.ParentBlock] and [Block.LastSettled]
// values, typically only required during database recovery or block
// verification. The source block MUST have the same hash as b.
//
// Although the individual ancestral blocks are shallow copied, calling
// [Block.MarkSettled] on either the source or destination will NOT clear the
// pointers of the other.
func (b *Block) CopyAncestorsFrom(c *Block) error {
	if from, to := c.Hash(), b.Hash(); from != to {
		return fmt.Errorf("%w: copying internals from block %#x to %#x", errHashMismatch, from, to)
	}
	a := c.ancestry.Load()
	return b.setAncestors(a.parent, a.lastSettled)
}

// A Source returns a [Block] that matches both a hash and number, and a boolean
// indicating if such a block was found.
type Source func(hash common.Hash, number uint64) (*Block, bool)

// EthBlock returns the [types.Block] with the given hash and number, or nil if
// not found.
func (s Source) EthBlock(h common.Hash, n uint64) *types.Block {
	b, ok := s(h, n)
	if !ok {
		return nil
	}
	return b.EthBlock()
}

// Header returns the [types.Header] with the given hash and number, or nil if
// not found.
func (s Source) Header(h common.Hash, n uint64) *types.Header {
	b, ok := s(h, n)
	if !ok {
		return nil
	}
	return b.Header()
}
