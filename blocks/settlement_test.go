package blocks

import (
	"time"

	"github.com/ava-labs/libevm/core/types"
)

func ExampleBlock_IfChildSettles() {
	parent := blockBuildingPreference()
	settle, ok := LastToSettleAt(uint64(time.Now().Unix()), parent)
	if !ok {
		return // execution is lagging; please come back soon
	}

	// Returns the (possibly empty) slice of blocks that may be settled by the
	// block being built.
	_ = parent.IfChildSettles(settle)
}

func blockBuildingPreference() *Block {
	b, _ := New(&types.Block{}, nil, nil, nil)
	return b
}
