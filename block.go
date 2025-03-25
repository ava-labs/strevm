package sae

import (
	"context"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/sync"
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

var errChans = sync.Pool[chan error]{
	New: func() chan error {
		return make(chan error)
	},
}

func (b *Block) Accept(ctx context.Context) error {
	return b.chain.accepted.Use(ctx, func(a *accepted) error {
		a.last = b.ID()
		a.all[b.ID()] = b

		errCh := errChans.Get()
		defer errChans.Put(errCh)
		// See the comment on [blockAcceptance] re temporary storage of a
		// Context, against recommended style.
		if !send(ctx.Done(), b.chain.toExecute, blockAcceptance{ctx, b, errCh}) {
			return ctx.Err()
		}
		return <-errCh
	})
}

func (b *Block) Reject(context.Context) error {
	return nil
}

func (b *Block) Parent() ids.ID {
	return ids.ID(b.b.ParentHash())
}

func (b *Block) Verify(ctx context.Context) error {
	// TODO(arr4n): this is where worst-case cost validation occurs.
	return b.chain.blocks.Use(ctx, func(bm blockMap) error {
		bm[b.ID()] = b
		return nil
	})
}

func (b *Block) Bytes() []byte {
	buf, err := rlp.EncodeToBytes(b.b)
	if err != nil {
		b.chain.logger().Error("rlp.EncodeToBytes()", zap.Error(err))
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
