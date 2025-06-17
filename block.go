package sae

import (
	"context"
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

func (vm *VM) AcceptBlock(ctx context.Context, b *blocks.Block) error {
	batch := vm.db.NewBatch()
	rawdb.WriteBlock(batch, b.Block)
	rawdb.WriteCanonicalHash(batch, b.Hash(), b.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, b.Block) // i.e. canonical tx inclusion

	settle := b.Settles()
	for i, s := range settle {
		if err := vm.exec.StateCache().TrieDB().Commit(s.PostExecutionStateRoot(), false); err != nil {
			return err
		}
		if i+1 == len(settle) {
			rawdb.WriteFinalizedBlockHash(batch, s.Hash())
		}
	}
	if err := b.WriteLastSettledNumber(batch); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}
	for _, s := range settle {
		s.MarkSettled()
	}

	// The documentation of invariants and ordering guarantees explicitly states
	// that these can happen in any order because they involve different blocks
	// entering different states.
	vm.last.settled.Store(b.LastSettled())
	vm.last.accepted.Store(b)

	// This MUST NOT happen before the database and [VM.last] are updated to
	// reflect that the block has been accepted.
	if err := vm.exec.EnqueueAccepted(ctx, b); err != nil {
		return err
	}
	// When the chain is bootstrapping, avalanchego expects to be able to call
	// `Verify` and `Accept` in a loop over blocks. Reporting an error during
	// either `Verify` or `Accept` is considered FATAL during this process.
	// Therefore, we must ensure that avalanchego does not get too far ahead of
	// the execution thread and FATAL during block verification.
	if vm.consensusState.Get() == snow.Bootstrapping {
		if err := b.WaitUntilExecuted(ctx); err != nil {
			return fmt.Errorf("waiting for block %d to execute: %v", b.Height(), err)
		}
	}

	vm.logger().Debug(
		"Accepted block",
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		// Same rationale as the invariant described in [Block]. Praised be the
		// GC!
		prune := func(b *blocks.Block) {
			delete(bm, b.Hash())
			vm.logger().Debug(
				"Pruning settled block",
				zap.Stringer("hash", b.Hash()),
				zap.Uint64("number", b.NumberU64()),
			)
		}

		keep := b.LastSettled().Hash()
		for _, s := range settle {
			if s.Hash() == keep {
				continue
			}
			prune(s)
		}
		parent := b.ParentBlock()
		if parent == nil {
			return nil
		}
		if s := parent.LastSettled(); s != nil && s.Hash() != keep {
			prune(s)
		}
		return nil
	})
}

func (vm *VM) RejectBlock(ctx context.Context, b *blocks.Block) error {
	// TODO(arr4n) add the transactions back to the mempool if necessary.
	return nil
}

func (vm *VM) ShouldVerifyBlockWithContext(ctx context.Context, b *blocks.Block) (bool, error) {
	return vm.hooks.ShouldVerifyBlockContext(ctx, b.Block)
}

func (vm *VM) VerifyBlockWithContext(ctx context.Context, blockContext *block.Context, b *blocks.Block) error {
	// Verify that the block is valid within the provided context. This must be
	// called even if the block was previously verified because the context may
	// be different.
	if err := vm.hooks.VerifyBlockContext(ctx, blockContext, b.Block); err != nil {
		return err
	}

	blockHash := b.Hash()
	var previouslyVerified bool
	// TODO(StephenButtolph): In the concurrency model this VM is implementing,
	// is this usage of the block map a logical race? The consensus engine
	// currently only ever calls VerifyWithContext and VerifyBlock on a single
	// thread, so the actual behavior seems correct.
	err := vm.blocks.Use(ctx, func(bm blockMap) error {
		_, previouslyVerified = bm[blockHash]
		return nil
	})
	if err != nil {
		return err
	}
	// If [VM.VerifyBlock] has already returned nil, we do not need to re-verify
	// the block.
	if previouslyVerified {
		return nil
	}
	return vm.VerifyBlock(ctx, b)
}

func (vm *VM) VerifyBlock(ctx context.Context, b *blocks.Block) error {
	parent, err := vm.GetBlock(ctx, ids.ID(b.ParentHash()))
	if err != nil {
		return fmt.Errorf("block parent %#x not found (presumed height %d)", b.ParentHash(), b.Height()-1)
	}

	ancestors := iterateUntilSettled(parent)
	if err := vm.hooks.VerifyBlockAncestors(ctx, b.Block, ancestors); err != nil {
		return err
	}

	signer := vm.signer(b.NumberU64(), b.Time())
	txs := b.Transactions()
	// This starts a concurrent, background pre-computation of the results of
	// [types.Sender], which is cached in each tx.
	core.SenderCacher.Recover(signer, b.Transactions())

	candidates := new(queue.FIFO[*pendingTx])
	candidates.Grow(txs.Len())
	for _, tx := range txs {
		from, err := types.Sender(signer, tx)
		if err != nil {
			return err
		}
		candidates.Push(&pendingTx{
			txAndSender: txAndSender{
				tx:   tx,
				from: from,
			},
		})
	}

	bb, err := vm.buildBlockWithCandidateTxs(b.Time(), parent, candidates)
	if err != nil {
		return err
	}
	if b.Hash() != bb.Hash() {
		// TODO(arr4n): add fine-grained checks to aid in debugging
		return errors.New("block built internally doesn't match one being verified")
	}
	if err := b.CopyAncestorsFrom(bb); err != nil {
		return err
	}

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.Hash()] = b
		return nil
	})
}

// iterateUntilSettled returns an iterator which starts at the provided block
// and iterates up to but not including the most recently settled block.
//
// If the provided block is settled, then the returned iterator is empty.
func iterateUntilSettled(from *blocks.Block) hook.BlockIterator {
	return func(yield func(*types.Block) bool) {
		for {
			next := from.ParentBlock()
			// If the next block is nil, then the current block is settled.
			if next == nil {
				return
			}

			// If the person iterating over this iterator broke out of the loop,
			// we must not call yield again.
			if !yield(from.Block) {
				return
			}

			from = next
		}
	}
}
