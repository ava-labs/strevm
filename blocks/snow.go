package blocks

import (
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
	"go.uber.org/zap"

	// Imported to allow IDE resolution of comments like [types.Block]. The
	// package is imported in other files so this is a no-op beyond devex.
	_ "github.com/ava-labs/libevm/core/types"
)

var _ adaptor.BlockProperties = (*Block)(nil)

// ID returns [types.Block.Hash] from the embedded [types.Block].
func (b *Block) ID() ids.ID {
	return ids.ID(b.Hash())
}

// Parent returns [types.Block.ParentHash] from the embedded [types.Block].
func (b *Block) Parent() ids.ID {
	return ids.ID(b.ParentHash())
}

// Bytes returns the RLP encoding of the embedded [types.Block]. If encoding
// returns an error, it is logged at the WARNING level and a nil slice is
// returned.
func (b *Block) Bytes() []byte {
	buf, err := rlp.EncodeToBytes(b)
	if err != nil {
		b.log.Warn("RLP encoding error", zap.Error(err))
		return nil
	}
	return buf
}

// Height returns [types.Block.NumberU64] from the embedded [types.Block].
func (b *Block) Height() uint64 {
	return b.NumberU64()
}

// Timestamp returns the timestamp of the embedded [types.Block], at
// [time.Second] resolution.
func (b *Block) Timestamp() time.Time {
	return time.Unix(int64(b.Time()), 0)
}
