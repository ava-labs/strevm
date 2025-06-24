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

var errBlockResettled = errors.New("block re-settled")

// MarkSettled marks the block as having being settled. This function MUST NOT
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
	if b.ancestry.CompareAndSwap(a, nil) {
		close(b.settled)
		return nil
	}
	b.log.Fatal("Block ancestry changed")
	// We have to return something to keen the compiler happy, even though we
	// expect the Fatal to be, well, fatal.
	return errors.New("block ancestry changed")
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

const (
	getParentOfSettledMsg  = "Get parent of settled block"
	getSettledOfSettledMsg = "Get last-settled of settled block"
)

// ParentBlock returns the block's parent unless [Block.MarkSettled] has been
// called, in which case it returns nil.
func (b *Block) ParentBlock() *Block {
	if a := b.ancestry.Load(); a != nil {
		return a.parent
	}
	b.log.Error(getParentOfSettledMsg)
	return nil
}

// LastSettled returns the last-settled block at the time of b's acceptance,
// unless [Block.MarkSettled] has been called, in which case it returns nil.
func (b *Block) LastSettled() *Block {
	if a := b.ancestry.Load(); a != nil {
		return a.lastSettled
	}
	b.log.Error(getSettledOfSettledMsg)
	return nil
}

// Settles returns the executed blocks that b settles if it is accepted by
// consensus. If `x` is the block height of the `b.ParentBlock().LastSettled()`
// and `y` is the height of the `b.LastSettled()`, then Settles returns the
// contiguous, half-open range (x,y] or an empty slice i.f.f. x==y.
//
// It is not valid to call Settles after a call to [Block.MarkSettled] on either
// b or its parent.
func (b *Block) Settles() []*Block {
	return b.ParentBlock().IfChildSettles(b.LastSettled())
}

// IfChildSettles is similar to [Block.Settles] but with different definitions
// of `x` and `y` (as described in [Block.Settles]). It is intended for use
// during block building and defines `x` as the block height of
// `b.LastSettled()` while `y` as the height of the argument passed to this
// method.
//
// The argument is typically the return value of [LastToSettleAt], where that
// function receives b as the parent. See the Example.
func (b *Block) IfChildSettles(lastSettledOfChild *Block) []*Block {
	return settling(b.LastSettled(), lastSettledOfChild)
}

// settling returns all the blocks after `lastOfParent` up to and including
// `lastOfCurr`, each of which are expected to be the block last-settled by a
// respective block-and-parent pair. It returns an empty slice if the two
// arguments have the same block hash.
func settling(lastOfParent, lastOfCurr *Block) []*Block {
	var settling []*Block
	for s := lastOfCurr; s.ParentBlock() != nil && s.Hash() != lastOfParent.Hash(); s = s.ParentBlock() {
		settling = append(settling, s)
	}
	slices.Reverse(settling)
	return settling
}

// LastToSettleAt returns the last block to be settled at time `settleAt` if
// building on the specified parent block, and a boolean to indicate if
// settlement is currently possible. If the returned boolean is false, the
// execution stream is lagging and LastToSettleAt can be called again after some
// indeterminate delay.
//
// See the Example for [Block.IfChildSettles] for one usage of the returned
// block.
func LastToSettleAt(settleAt uint64, parent *Block) (*Block, bool) {
	// These variables are only abstracted for clarity; they are not needed
	// beyond the scope of the `for` loop.
	var block, child *Block
	block = parent // therefore `child` remains nil

	// The only way [Block.parent] can be nil is if it was already settled (see
	// invariant in [Block]). If a block was already settled then only that or a
	// later (i.e. unsettled) block can be returned by this loop, therefore we
	// have a guarantee that the loop update will never result in `block==nil`.
	// Framed differently, because `settleAt` is >= what it was when this
	// function was used to build `parent`, if `block.parent==nil` then it will
	// have already been settled in `parent` and therefore executed by
	// `<=settleAt` so will be returned here.
	for ; ; block, child = block.ParentBlock(), block {
		if block.Time() > settleAt {
			continue
		}

		if t := block.executionExceededSecond.Load(); t != nil && *t >= settleAt {
			continue
		}
		if e := block.execution.Load(); e != nil {
			if e.byGas.CmpUnix(settleAt) > 0 {
				// Although this check is redundant because of the similar one
				// just above, it's fast so there's no harm in double-checking.
				continue
			}
			return block, true
		}

		// TODO(arr4n) more fine-grained checks are possible for scenarios where
		// (a) `block` could never execute before `settleAt` so we would
		// `continue`; and (b) `block` will definitely execute in time and
		// `child` could never, in which case return `nil, false`.
		_ = child

		return nil, false
	}
}
