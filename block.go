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

type Block = blocks.Block

func (vm *VM) AcceptBlock(ctx context.Context, b *Block) error {
	if err := vm.exec.enqueueAccepted(ctx, b); err != nil {
		return err
	}

	// When the chain is bootstrapping, avalanchego expects to be able to call
	// `Verify` and `Accept` in a loop over blocks. Reporting an error during
	// either `Verify` or `Accept` is considered FATAL during this process.
	// Therefore, we must ensure that avalanchego does not get too far ahead of
	// the execution thread and FATAL during block verification.
	if vm.consensusState.Get() == snow.Bootstrapping {
		if err := vm.exec.queueCleared.Wait(ctx); err != nil {
			return fmt.Errorf("waiting for execution during bootstrap: %v", err)
		}
	}

	batch := vm.db.NewBatch()
	rawdb.WriteBlock(batch, b.Block)
	rawdb.WriteCanonicalHash(batch, b.Hash(), b.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, b.Block) // i.e. canonical tx inclusion

	settle := b.Settles()
	for i, s := range settle {
		if err := vm.exec.stateCache.TrieDB().Commit(s.PostExecutionStateRoot(), false); err != nil {
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

	vm.last.settled.Store(b.LastSettled())
	vm.last.accepted.Store(b)

	vm.logger().Debug(
		"Accepted block",
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		// Same rationale as the invariant described in [Block]. Praised be the
		// GC!
		prune := func(b *Block) {
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

func (vm *VM) RejectBlock(ctx context.Context, b *Block) error {
	rawdb.DeleteBlock(vm.db, b.Hash(), b.NumberU64())
	// TODO(arr4n) add the transactions back to the mempool if necessary.
	return nil
}

func (vm *VM) VerifyBlock(ctx context.Context, b *Block) error {
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

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.Hash()] = b
		return nil
	})
}
