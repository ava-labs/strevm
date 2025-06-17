package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	"github.com/ava-labs/libevm/core/types"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
)

const (
	TargetGasPerSecond = 1_000_000
)

type hooks struct{}

func (*hooks) GasTarget(parent *types.Block) gas.Gas {
	return TargetGasPerSecond
}

func (*hooks) ShouldVerifyBlockContext(context.Context, *types.Block) (bool, error) {
	return false, nil
}

func (*hooks) VerifyBlockContext(context.Context, *block.Context, *types.Block) error {
	return nil
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
