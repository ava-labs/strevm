// Package blocks defines [Streaming Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blocks

import (
	"fmt"
	"math"
	"runtime"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"go.uber.org/zap"
)

// A Block extends a [types.Block] to track SAE-defined concepts of async
// execution and settlement. It MUST be constructed with [New].
type Block struct {
	*types.Block
	// Invariant: ancestry is non-nil and contains non-nil pointers i.f.f. the
	// block hasn't itself been settled. A synchronous block (e.g. SAE genesis
	// or the last pre-SAE block) is always considered settled.
	//
	// Rationale: the ancestral pointers form a linked list that would prevent
	// garbage collection if not severed. Once a block is settled there is no
	// need to inspect its history so we sacrifice the ancestors to the GC
	// Overlord as a sign of our unwavering fealty.
	ancestry  atomic.Pointer[ancestry]
	execution atomic.Pointer[executionResults]

	log logging.Logger
}

var inMemoryBlockCount atomic.Uint64

// InMemoryBlockCount returns the number of blocks created with [New] that are
// yet to have their GC finalizers run.
func InMemoryBlockCount() uint64 {
	return inMemoryBlockCount.Load()
}

// New constructs a new Block.
func New(eth *types.Block, parent, lastSettled *Block, log logging.Logger) (*Block, error) {
	b := &Block{
		Block: eth,
	}
	// TODO(arr4n) change to runtime.AddCleanup after the Go version has been
	// bumped to >=1.24.0.
	inMemoryBlockCount.Add(1)
	runtime.SetFinalizer(b, func(*Block) {
		inMemoryBlockCount.Add(math.MaxUint64) // -1
	})

	if err := b.setAncestors(parent, lastSettled); err != nil {
		return nil, err
	}
	b.log = log.With(
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)
	return b, nil
}

func (b *Block) setAncestors(parent, lastSettled *Block) error {
	if parent != nil {
		if got, want := parent.Hash(), b.ParentHash(); got != want {
			return fmt.Errorf("constructing Block with parent hash %v; expecting %v", got, want)
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
func (b *Block) CopyAncestorsFrom(c *Block) error {
	if from, to := c.Hash(), b.Hash(); from != to {
		return fmt.Errorf("copying internals from block %#x to %#x", from, to)
	}
	a := c.ancestry.Load()
	return b.setAncestors(a.parent, a.lastSettled)
}
