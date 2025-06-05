package sae

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/holiman/uint256"
)

type Hooks interface {
	UpdateGasParams(parent *types.Block, current *GasParams)
}

func (c *gasClock) maxQueueLength() gas.Gas {
	return c.params.R * stateRootDelaySeconds * lambda
}

func (c *gasClock) maxBlockSize() gas.Gas {
	return c.params.R * maxGasSecondsPerBlock
}

func (p *GasParams) baseFee() *uint256.Int {
	return uint256.NewInt(uint64(p.Price))
}
