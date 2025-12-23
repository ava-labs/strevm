// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"math/big"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/hook"
)

// consensusHooks implements [hook.Points].
type consensusHooks struct {
	consensus consensus.Engine
	reader    *readerAdapter
}

var _ hook.Points = (*consensusHooks)(nil)

func newTestConsensusHooks(consensus consensus.Engine, reader *readerAdapter) *consensusHooks {
	return &consensusHooks{consensus: consensus, reader: reader}
}

// GasTarget ignores its argument and always returns [consensusHooks.Target].
func (c *consensusHooks) GasTargetAfter(*types.Header) gas.Gas {
	return 1e6
}

// SubSecondBlockTime time ignores its argument and always returns 0.
func (*consensusHooks) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return 0
}

// EndOfBlockOps is a no-op.
func (*consensusHooks) EndOfBlockOps(*types.Block) []hook.Op {
	return nil
}

var _ core.ChainContext = (*chainContext)(nil)

type chainContext struct {
	engine consensus.Engine
	*readerAdapter
}

func (c *chainContext) Engine() consensus.Engine {
	return c.engine
}

// BeforeExecutingBlock processes the beacon block root if present.
func (c *consensusHooks) BeforeExecutingBlock(_ params.Rules, statedb *state.StateDB, b *types.Block) error {
	if beaconRoot := b.BeaconRoot(); beaconRoot != nil {
		chainContext := &chainContext{engine: c.consensus, readerAdapter: c.reader}
		context := core.NewEVMBlockContext(b.Header(), chainContext, nil)
		vmenv := vm.NewEVM(context, vm.TxContext{}, statedb, chainContext.Config(), vm.Config{})
		core.ProcessBeaconBlockRoot(*beaconRoot, vmenv, statedb)
	}
	return nil
}

// AfterExecutingBlock finalizes the block and updates the total difficulty.
func (c *consensusHooks) AfterExecutingBlock(statedb *state.StateDB, b *types.Block, receipts types.Receipts) {
	currentNumber := b.NumberU64()
	currentTd := big.NewInt(0)
	if currentNumber > 0 {
		currentTd = c.reader.GetTd(b.ParentHash(), currentNumber-1)
		if currentTd == nil {
			currentTd = big.NewInt(0)
			if c.reader.logger != nil {
				c.reader.logger.Error("currentTd is nil")
			}
		}
	}
	newTd := new(big.Int).Add(currentTd, b.Difficulty())
	c.reader.SetTd(b.Hash(), b.NumberU64(), newTd.Uint64())
	c.consensus.Finalize(c.reader, b.Header(), statedb, b.Transactions(), b.Uncles(), b.Withdrawals())
}
