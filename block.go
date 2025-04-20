package sae

import (
	"context"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/ids"
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
	tranche *txTranche
}

func (b *Block) ID() ids.ID {
	return ids.ID(b.Hash())
}

func (vm *VM) AcceptBlock(ctx context.Context, b *Block) error {
	if err := vm.accepted.Use(ctx, func(a *accepted) error {
		parent := a.last() // nil i.f.f. `b` is genesis, but that's allowed
		if err := vm.exec.enqueueAccepted(ctx, b, parent); err != nil {
			return err
		}

		a.all[b.ID()] = b
		a.lastID = b.ID()
		a.heightToID[b.NumberU64()] = b.ID()

		vm.logger().Debug(
			"Accepted block",
			zap.Uint64("height", b.Height()),
		)
		return nil
	}); err != nil {
		return err
	}

	// Synchronises our [blockBuilder] with those of other validators.
	return vm.builder.acceptTranche(ctx, b.tranche)
}

func (*VM) RejectBlock(context.Context, *Block) error {
	// TODO(arr4n) add the transactions back to the mempool if necessary.
	return nil
}

func (b *Block) Parent() ids.ID {
	return ids.ID(b.ParentHash())
}

func (vm *VM) VerifyBlock(ctx context.Context, b *Block) error {
	x := &vm.exec.executeScratchSpace // TODO(arr4n) don't access this directly
	signer := types.LatestSigner(x.chainConfig)

	txs := b.Transactions()
	// This starts a concurrent, background pre-computation of the results of
	// [types.Sender], which is cached in each tx.
	core.SenderCacher.Recover(signer, txs)
	candidates := new(queue.FIFO[*transaction])
	candidates.Grow(txs.Len())
	for _, tx := range txs {
		from, err := types.Sender(signer, tx)
		if err != nil {
			return err
		}
		candidates.Push(&transaction{
			tx:   tx,
			from: from,
		})
	}

	// While block-building tranches sample from the mempool, here we use the
	// unverified block's transactions as the candidates. If the tranche adds
	// all txs then (a) the block is valid; and (b) our local [blockBuilder]
	// will be in sync with all peers' (including the proposer) should this
	// block be accepted.
	cfg := &trancheBuilderConfig{
		atEndOf: &chunk{
			timestamp:     clippedSubtract(b.Time(), stateRootDelaySeconds),
			stateRootPost: b.Root(),
		},
		candidates: candidates,
		gasConfig:  &x.gasConfig,
	}
	tranche, err := vm.builder.makeTranche(ctx, cfg)
	if err != nil {
		return err
	}
	if nTranche, nBlock := len(tranche.rawTxs), len(txs); nTranche != nBlock {
		return fmt.Errorf("validation %T has %d proposed txs from block's %d", tranche, nTranche, nBlock)
	}
	for i, bTx := range txs {
		if bTx.Hash() != tranche.rawTxs[i].Hash() {
			return fmt.Errorf("block and validation %T have mismatched tx[%d]", tranche, i)
		}
	}

	// The tranche will be appended to the builder's i.f.f. [Block.Accept] is
	// called later, but for now it matches the proposer's tranche from
	// block-building.
	b.tranche = tranche
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.ID()] = b
		return nil
	})
}

func (b *Block) Bytes() []byte {
	buf, err := rlp.EncodeToBytes(b)
	if err != nil {
		// b.chain.logger().Error("rlp.EncodeToBytes(Block)", zap.Error(err))
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
