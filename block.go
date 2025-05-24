package sae

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

var _ adaptor.Block = (*Block)(nil)

type Block struct {
	*types.Block
	parent, lastSettled *Block

	accepted, executed atomic.Bool

	execution struct { // valid and immutable i.f.f. `executed == true`
		by            gasClock
		byTime        time.Time
		receipts      types.Receipts
		stateRootPost common.Hash
	}
}

func (b *Block) ID() ids.ID {
	return ids.ID(b.Hash())
}

func (vm *VM) AcceptBlock(ctx context.Context, b *Block) error {
	return vm.accepted.Use(ctx, func(a *accepted) error {
		if err := vm.exec.enqueueAccepted(ctx, b); err != nil {
			return err
		}

		a.all[b.ID()] = b
		a.lastID = b.ID()
		a.heightToID[b.NumberU64()] = b.ID()

		b.accepted.Store(true)

		vm.logger().Debug(
			"Accepted block",
			zap.Uint64("height", b.Height()),
			zap.Stringer("hash", b.Hash()),
		)
		return nil
	})
}

func (*VM) RejectBlock(context.Context, *Block) error {
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

	signer := vm.signer()
	txs := b.Transactions()
	// This starts a concurrent, background pre-computation of the results of
	// [types.Sender], which is cached in each tx.
	core.SenderCacher.Recover(vm.signer(), b.Transactions())

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

	return vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.ID()] = b
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
