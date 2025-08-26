package hooktest

import (
	"context"
	"iter"
	"testing"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/intmath"
	"github.com/stretchr/testify/require"
)

type Simple struct {
	T gas.Gas
}

func (s Simple) GasTarget(parent *types.Block) gas.Gas {
	return s.T
}

func (Simple) ConstructBlock(
	ctx context.Context,
	blockContext *block.Context,
	header *types.Header,
	parent *types.Header,
	ancestors iter.Seq[*types.Block],
	state hook.State,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) (*types.Block, error) {
	return types.NewBlock(
		header,
		txs,
		nil, /*uncles*/
		receipts,
		trie.NewStackTrie(nil),
	), nil
}

func (s Simple) ConstructBlockFromBlock(ctx context.Context, block *types.Block) (hook.ConstructBlock, error) {
	return s.ConstructBlock, nil
}

func (Simple) ExtraBlockOperations(ctx context.Context, block *types.Block) ([]hook.Op, error) {
	return nil, nil
}

func (Simple) BlockExecuted(ctx context.Context, block *types.Block, receipts types.Receipts) error {
	return nil
}

func (s Simple) FractionSecondsOfGas(tb testing.TB, num, denom uint64) gas.Gas {
	tb.Helper()
	quo, rem, err := intmath.MulDiv(gas.Gas(num), 2*s.T, gas.Gas(denom))
	require.NoError(tb, err)
	require.Zero(tb, rem, "remainder when calculating fractional seconds of gas")
	return quo
}
