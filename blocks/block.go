// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blocks defines [Streaming Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blocks

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"go.uber.org/zap"
)

// A Block extends a [types.Block] to track SAE-defined concepts of async
// execution and settlement. It MUST be constructed with [New].
type Block struct {
	b *types.Block
	// Invariant: ancestry is non-nil and contains non-nil pointers i.f.f. the
	// block hasn't itself been settled. A synchronous block (e.g. SAE genesis
	// or the last pre-SAE block) is always considered settled.
	//
	// Rationale: the ancestral pointers form a linked list that would prevent
	// garbage collection if not severed. Once a block is settled there is no
	// need to inspect its history so we sacrifice the ancestors to the GC
	// Overlord as a sign of our unwavering fealty. See [InMemoryBlockCount] for
	// observability.
	ancestry atomic.Pointer[ancestry]
	// Non-nil i.f.f. [Block.MarkExecuted] or [Block.ResotrePostExecutionState]
	// have returned without error.
	execution atomic.Pointer[executionResults]

	// See [Block.SetInterimExecutionTime for setting and [LastToSettleAt] for
	// usage. The pointer MAY be nil if execution is yet to commence.
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
	errParentHashMismatch = errors.New("block-parent hash mismatch")
	errHashMismatch       = errors.New("block hash mismatch")
)

func (b *Block) setAncestors(parent, lastSettled *Block) error {
	if parent != nil {
		if got, want := parent.Hash(), b.ParentHash(); got != want {
			return fmt.Errorf("%w: constructing Block with parent hash %v; expecting %v", errParentHashMismatch, got, want)
		}
	}
	b.ancestry.Store(&ancestry{
		parent:      parent,
		lastSettled: lastSettled,
	})
	return nil
}

// CopyAncestorsFrom populates the [Block.ParentBlock] and [Block.LastSettled]
// values, typically only required during database recovery. The source block
// MUST have the same hash as b.
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
