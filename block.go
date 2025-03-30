package sae

import (
	"context"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"
)

func init() {
	var (
		block *Block
		_     snow.Decidable = block
		_     snowman.Block  = block
	)
}

type Block struct {
	b     *types.Block
	chain *Chain
}

func (b *Block) ID() ids.ID {
	return ids.ID(b.b.Hash())
}

func (b *Block) Accept(ctx context.Context) error {
	// TODO(arr4n) if the block wasn't built locally then the txs need to be
	// pushed to the [blockBuilder] pending queue. See the equivalent comment at
	// the beginning of [Block.Reject] for more thoughts.
	return b.chain.accepted.Use(ctx, func(a *accepted) error {
		parent := a.last() // nil i.f.f. `b` is genesis, but that's allowed
		if err := b.chain.exec.enqueueAccepted(ctx, b, parent); err != nil {
			return err
		}

		a.all[b.ID()] = b
		a.lastID = b.ID()

		b.chain.logger().Debug(
			"Accepted block",
			zap.Uint64("height", b.Height()),
		)
		return nil
	})
}

func (b *Block) Reject(context.Context) error {
	// TODO(arr4n) if the block was built locally then it will be in the pending
	// queue of the [blockBuilder] so needs to be removed. The likely best fix
	// for this is to have the block builder split its current queue into a base
	// (accepted) queue and others (proposed) to be appended if accepted,
	// similar to layers in a snapshot tree. This will avoid needing to actually
	// remove anything.
	return nil
}

func (b *Block) Parent() ids.ID {
	return ids.ID(b.b.ParentHash())
}

func (b *Block) Verify(ctx context.Context) error {
	// TODO(arr4n): mirror the worst-case validity checks as done by
	// [blockBuilder].
	return b.chain.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.ID()] = b
		return nil
	})
}

func (b *Block) Bytes() []byte {
	buf, err := rlp.EncodeToBytes(b.b)
	if err != nil {
		b.chain.logger().Error("rlp.EncodeToBytes(Block)", zap.Error(err))
		return nil
	}
	return buf
}

func (b *Block) Height() uint64 {
	return b.b.NumberU64()
}

func (b *Block) Timestamp() time.Time {
	return time.Unix(int64(b.b.Time()), 0)
}
