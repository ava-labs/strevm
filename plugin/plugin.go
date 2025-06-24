package main

import (
	"context"
	"fmt"
	"iter"
	"os"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/hook"
)

const (
	TargetGasPerSecond = 1_000_000
)

type hooks struct{}

func (*hooks) GasTarget(parent *types.Block) gas.Gas {
	return TargetGasPerSecond
}

func (*hooks) ExtraBlockOperations(ctx context.Context, block *types.Block) ([]hook.Op, error) {
	return nil, nil
}

func (h *hooks) ConstructBlock(
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
		txs, nil, /*uncles*/
		receipts,
		trie.NewStackTrie(nil),
	), nil
}

func (h *hooks) ConstructBlockFromBlock(ctx context.Context, block *types.Block) (hook.ConstructBlock, error) {
	return h.ConstructBlock, nil
}

func main() {
	vm := adaptor.Convert(&sae.SinceGenesis{
		Hooks: &hooks{},
	})

	if err := rpcchainvm.Serve(context.Background(), vm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
