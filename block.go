package sae

import (
	"context"
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

func (vm *VM) newBlock(b *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	return blocks.New(b, parent, lastSettled, vm.logger())
}

func (vm *VM) addBlocksToMemory(ctx context.Context, bs ...*blocks.Block) error {
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		for _, b := range bs {
			bm[b.Hash()] = b
		}
		return nil
	})
}

func (vm *VM) removeBlocksFromMemory(ctx context.Context, bs ...*blocks.Block) error {
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		for _, b := range bs {
			delete(bm, b.Hash())
		}
		return nil
	})
}

func (vm *VM) AcceptBlock(ctx context.Context, b *blocks.Block) error {
	if err := vm.maybeCommitTrieDB(b); err != nil {
		return err
	}

	settles := b.Settles()
	{
		batch := vm.db.NewBatch()

		rawdb.WriteBlock(batch, b.Block)
		rawdb.WriteCanonicalHash(batch, b.Hash(), b.NumberU64())
		rawdb.WriteTxLookupEntriesByBlock(batch, b.Block) // i.e. canonical tx inclusion

		if s := settles; len(s) > 0 {
			rawdb.WriteFinalizedBlockHash(batch, s[len(s)-1].Hash())
		}
		if err := b.WriteLastSettledNumber(batch); err != nil {
			return err
		}

		if err := batch.Write(); err != nil {
			return err
		}
	}
	for _, s := range settles {
		if err := s.MarkSettled(); err != nil {
			return err
		}
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

	// Same rationale as the invariant described in [Block]. Praised be the GC!
	var toPrune []*blocks.Block
	keep := b.LastSettled().Hash()
	for _, s := range settles {
		if s.Hash() == keep {
			continue
		}
		toPrune = append(toPrune, s)
	}
	if p := b.ParentBlock(); p != nil {
		if s := p.LastSettled(); s != nil && s.Hash() != keep {
			toPrune = append(toPrune, s)
		}
	}
	return vm.removeBlocksFromMemory(ctx, toPrune...)
}

func (vm *VM) RejectBlock(ctx context.Context, b *blocks.Block) error {
	// TODO(arr4n) add the transactions back to the mempool if necessary.
	return vm.removeBlocksFromMemory(ctx, b)
}

func (vm *VM) VerifyBlock(ctx context.Context, b *blocks.Block) error {
	parent, err := vm.GetBlock(ctx, ids.ID(b.ParentHash()))
	if err != nil {
		return fmt.Errorf("block parent %#x not found (presumed height %d)", b.ParentHash(), b.Height()-1)
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
	return vm.addBlocksToMemory(ctx, b)
}
