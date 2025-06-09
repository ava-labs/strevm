package sae

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

var _ adaptor.Block = (*Block)(nil)

type Block struct {
	*types.Block
	// Invariant: `parent` and `lastSettled` are non-nil i.f.f. the block hasn't
	// itself been settled. However their non-atomic nature means that this is
	// not a safe way to check if the block has been settled; the invariant is
	// intended only for guiding other code. The last synchronous block, the SAE
	// "genesis" is always considered settled.
	//
	// Rationale: the ancestral pointers form a linked list that would prevent
	// garbage collection if not severed. Once a block is settled there is no
	// need to inspect its history so we sacrifice the ancestors to the GC
	// Overlord as a sign of our unwavering fealty.
	parent, lastSettled *Block

	executed  atomic.Bool
	execution *executionResults // non-nil and immutable i.f.f. `executed == true`
}

func (b *Block) ID() ids.ID {
	return ids.ID(b.Hash())
}

func (vm *VM) AcceptBlock(ctx context.Context, b *Block) error {
	if err := vm.exec.enqueueAccepted(ctx, b); err != nil {
		return err
	}

	// When the chain is bootstrapping, avalanchego expects to be able to call
	// `Verify` and `Accept` in a loop over blocks. Reporting an error during
	// either `Verify` or `Accept` is considered FATAL during this process.
	// Therefore, we must ensure that avalanchego does not get too far ahead of
	// the execution thread and FATAL during block Verification.
	if vm.consensusState.Get() == snow.Bootstrapping {
		if err := vm.exec.awaitEmpty(ctx); err != nil {
			return err
		}
	}

	batch := vm.db.NewBatch()
	rawdb.WriteCanonicalHash(batch, b.Hash(), b.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, b.Block) // i.e. canonical tx inclusion

	settle := b.settles()
	for i, s := range settle {
		if err := vm.exec.stateCache.TrieDB().Commit(s.execution.stateRootPost, false); err != nil {
			return err
		}
		if i+1 == len(settle) {
			rawdb.WriteFinalizedBlockHash(batch, s.Hash())
		}
	}
	if err := b.writeLastSettledNumber(batch); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}
	for _, s := range settle {
		// See the invariant detailed in [Block].
		s.parent = nil
		s.lastSettled = nil
	}

	vm.last.settled.Store(b.lastSettled)
	vm.last.accepted.Store(b)

	vm.logger().Info(
		"Accepted block",
		zap.Uint64("height", b.Height()),
		zap.Stringer("hash", b.Hash()),
	)

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		// Same rationale as the invariant described in [Block]. Praised be the
		// GC!
		prune := func(b *Block) {
			delete(bm, b.Hash())
			vm.logger().Info(
				"Pruning settled block",
				zap.Stringer("hash", b.Hash()),
				zap.Uint64("number", b.NumberU64()),
			)
		}

		keep := b.lastSettled.Hash()
		for _, s := range settle {
			if s.Hash() == keep {
				continue
			}
			prune(s)
		}
		if b.parent == nil {
			return nil
		}
		if s := b.parent.lastSettled; s != nil && s.Hash() != keep {
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

func (b *Block) Parent() ids.ID {
	return ids.ID(b.ParentHash())
}

func (vm *VM) VerifyBlock(ctx context.Context, b *Block) error {
	parent, err := vm.GetBlock(ctx, ids.ID(b.ParentHash()))
	if err != nil {
		return fmt.Errorf("block parent %#x not found (presumed height %d)", b.ParentHash(), b.Height()-1)
	}
	b.parent = parent

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

	b.lastSettled = bb.lastSettled

	rawdb.WriteBlock(vm.db, b.Block)
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.Hash()] = b
		return nil
	})
}

func (b *Block) Bytes() []byte {
	buf, err := rlp.EncodeToBytes(b)
	if err != nil {
		return nil
	}
	return buf
}

func (b *Block) Height() uint64 {
	return b.NumberU64()
}

func (b *Block) Timestamp() time.Time {
	return time.Unix(int64(b.Time()), 0)
}

// settles returns the executed blocks that `b` settles. If `x` is the block
// height of the last-settled block of b's parent and `y` is the height of the
// last-settled of `b`, then settles returns blocks in the half-open range (x,y]
// or an empty slice iff x==y.
func (b *Block) settles() []*Block {
	return settling(b.parent.lastSettled, b.lastSettled)
}

// settling returns all the blocks after `lastOfParent` up to and including
// `lastOfCurr`, each of which are expected to be the block last-settled by a
// respective block-and-parent pair. It returns an empty slice if the two
// arguments have the same block hash.
func settling(lastOfParent, lastOfCurr *Block) []*Block {
	var settling []*Block
	for s := lastOfCurr; s.parent != nil && s.Hash() != lastOfParent.Hash(); s = s.parent {
		settling = append(settling, s)
	}
	slices.Reverse(settling)
	return settling
}
