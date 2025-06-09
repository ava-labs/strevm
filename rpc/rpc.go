package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	"github.com/ava-labs/libevm/core/types"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
)

const (
	TargetGasPerSecond       = 1_000_000
	GasCapacityPerSecond     = 2 * TargetGasPerSecond
	ExcessConversionConstant = 87 * TargetGasPerSecond
)

type hooks struct{}

func (h *hooks) UpdateGasParams(parent *types.Block, p *sae.GasParams) {
	*p = sae.GasParams{
		T:      TargetGasPerSecond,
		R:      GasCapacityPerSecond,
		Price:  gas.CalculatePrice(1, p.Excess, ExcessConversionConstant),
		Excess: p.Excess,
	}
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
