// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

type ancestry struct {
	parent, lastSettled *Block
}

var (
	errBlockResettled       = errors.New("block re-settled")
	errBlockAncestryChanged = errors.New("block ancestry changed during settlement")
)

// MarkSettled marks the block as having been settled. This function MUST NOT
// be called more than once.
//
// After a call to MarkSettled, future calls to [Block.ParentBlock] and
// [Block.LastSettled] will return nil.
func (b *Block) MarkSettled() error {
	a := b.ancestry.Load()
	if a == nil {
		b.log.Error(errBlockResettled.Error())
		return fmt.Errorf("%w: block height %d", errBlockResettled, b.Height())
	}
	if !b.ancestry.CompareAndSwap(a, nil) { // almost certainly means concurrent calls to this method
		b.log.Fatal("Block ancestry changed during settlement")
		// We have to return something to keen the compiler happy, even though we
		// expect the Fatal to be, well, fatal.
		return errBlockAncestryChanged
	}
	close(b.settled)
	return nil
}

// MarkSynchronous is a special case of [Block.MarkSettled], reserved for the
// last pre-SAE block, which MAY be the genesis block. These are, by definition,
// self-settling so require special treatment as such behaviour is impossible
// under SAE rules.
func (b *Block) MarkSynchronous() error {
	b.synchronous = true
	return b.MarkSettled()
}

// WaitUntilSettled blocks until either [Block.MarkSettled] is called or the
// [context.Context] is cancelled.
func (b *Block) WaitUntilSettled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.settled:
		return nil
	}
}

func (b *Block) ancestor(ifSettledErrMsg string, get func(*ancestry) *Block) *Block {
	a := b.ancestry.Load()
	if a == nil {
		b.log.Error(ifSettledErrMsg)
		return nil
	}
	return get(a)
}

const (
	getParentOfSettledErrMsg  = "Get parent of settled block"
	getSettledOfSettledErrMsg = "Get last-settled of settled block"
)

// ParentBlock returns the block's parent unless [Block.MarkSettled] has been
// called, in which case it returns nil.
func (b *Block) ParentBlock() *Block {
	return b.ancestor(getParentOfSettledErrMsg, func(a *ancestry) *Block {
		return a.parent
	})
}

// LastSettled returns the last-settled block at the time of b's acceptance,
// unless [Block.MarkSettled] has been called, in which case it returns nil.
// Note that this value might not be distinct between contiguous blocks.
func (b *Block) LastSettled() *Block {
	if b.synchronous {
		return b
	}
	return b.ancestor(getSettledOfSettledErrMsg, func(a *ancestry) *Block {
		return a.lastSettled
	})
}

// Settles returns the executed blocks that b settles if it is accepted by
// consensus. If `x` is the block height of the `b.ParentBlock().LastSettled()`
// and `y` is the height of the `b.LastSettled()`, then Settles returns the
// contiguous, half-open range (x,y] or an empty slice i.f.f. x==y. Every block
// therefore returns a disjoint (and possibly empty) set of historical blocks.
//
// It is not valid to call Settles after a call to [Block.MarkSettled] on either
// b or its parent.
func (b *Block) Settles() []*Block {
	if b.synchronous {
		return []*Block{b}
	}
	return settling(b.ParentBlock().LastSettled(), b.LastSettled())
}

// WhenChildSettles returns the blocks that would be settled by a child of `b`,
// given the last-settled block at that child's block time. Note that the
// last-settled block at the child's time MAY be equal to the last-settled of
// `b` (its parent), in which case WhenChildSettles returns an empty slice.
//
// The argument is typically the return value of [LastToSettleAt], where that
// function receives `b` as the parent. See the Example.
//
// WhenChildSettles MUST only be called before the call to [Block.MarkSettled]
// on `b`. The intention is that this method is called on the VM's preferred
// block, which always meets this criterion. This is by definition of
// settlement, which requires that at least one descendant block has already
// been accepted, which the preference never has.
//
// WhenChildSettles is similar to [Block.Settles] but with different definitions
// of `x` and `y` (as described in [Block.Settles]). It is intended for use
// during block building and defines `x` as the block height of
// `b.LastSettled()` while `y` as the height of the argument passed to this
// method.
func (b *Block) WhenChildSettles(lastSettledOfChild *Block) []*Block {
	return settling(b.LastSettled(), lastSettledOfChild)
}

// settling returns all the blocks after `lastOfParent` up to and including
// `lastOfCurr`, each of which are expected to be the block last-settled by a
// respective block-and-parent pair. It returns an empty slice if the two
// arguments have the same block hash.
func settling(lastOfParent, lastOfCurr *Block) []*Block {
	var settling []*Block
	// TODO(arr4n) abstract this to combine functionality with iterators
	// introduced by @StephenButtolph.
	for s := lastOfCurr; s.Hash() != lastOfParent.Hash(); s = s.ParentBlock() {
		settling = append(settling, s)
	}
	slices.Reverse(settling)
	return settling
}

// LastToSettleAt returns (a) the last block to be settled at time `settleAt` if
// building on the specified parent block, and (b) a boolean to indicate if
// settlement is currently possible. If the returned boolean is false, the
// execution stream is lagging and LastToSettleAt can be called again after some
// indeterminate delay.
//
// See the Example for [Block.WhenChildSettles] for one usage of the returned
// block.
func LastToSettleAt(settleAt uint64, parent *Block) (*Block, bool) {
	// A block can be the last to settle at some time i.f.f. two criteria are
	// met:
	//
	// 1. The block has finished execution by said time and;
	//
	// 2. The block's child is known to have *not* finished execution or be
	//    unable to finish by that time.
	//
	// The block currently being built can never finish in time, so we start
	// with criterion (2) being met.
	known := true

	// The only way [Block.ParentBlock] can be nil is if `block` was already
	// settled (see invariant in [Block]). If a block was already settled then
	// only that or a later (i.e. unsettled) block can be returned by this loop,
	// therefore we have a guarantee that the loop update will never result in
	// `block==nil`.
	for block := parent; ; block = block.ParentBlock() {
		if startsNoEarlierThan := block.BuildTime(); startsNoEarlierThan > settleAt {
			known = true
			continue
		}
		// TODO(arr4n) more fine-grained checks are possible by computing the
		// minimum possible gas consumption of blocks. For example,
		// `block.BuildTime()+block.intrinsicGasSum()` can be compared against
		// `settleAt`, as can the sum of a chain of blocks.

		if t := block.executionExceededSecond.Load(); t != nil && *t >= settleAt {
			known = true
			continue
		}
		if e := block.execution.Load(); e != nil {
			if e.byGas.CompareUnix(settleAt) > 0 {
				// There may have been a race between this check and the
				// execution-exceeded one above, so we have to check again.
				known = true
				continue
			}
			return block, known
		}

		// Note that a grandchild block having unknown execution completion time
		// does not rule out knowing a child's completion time, so this could be
		// set to true in a future loop iteration.
		known = false
	}
}
