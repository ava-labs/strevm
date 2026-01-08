// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
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
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/hook"
)

// defaultTestGasTarget is the gas target used for ethereum JSON tests.
const defaultTestGasTarget = gas.Gas(1e6)

// testHooks implements [hook.Points] for ethereum JSON tests.
// It handles both state tests (consensus == nil) and block tests (consensus != nil).
type testHooks struct {
	consensus consensus.Engine // nil for state tests
	reader    *readerAdapter
}

var _ hook.Points = (*testHooks)(nil)

// newTestHooks creates hooks for ethereum JSON tests.
// For block tests, pass a consensus engine; for state tests, pass nil.
func newTestHooks(engine consensus.Engine, reader *readerAdapter) *testHooks {
	return &testHooks{consensus: engine, reader: reader}
}

// BuildBlock returns nil as test blocks are built externally.
func (*testHooks) BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) *types.Block {
	return nil
}

// BuildHeader returns nil as test headers are built externally.
func (*testHooks) BuildHeader(parent *types.Header) *types.Header {
	return nil
}

// BlockRebuilderFrom returns itself as the block rebuilder.
func (t *testHooks) BlockRebuilderFrom(block *types.Block) hook.BlockBuilder {
	return t
}

// GasTargetAfter returns the default test gas target.
func (*testHooks) GasTargetAfter(*types.Header) gas.Gas {
	return defaultTestGasTarget
}

// SubSecondBlockTime returns 0 as ethereum tests don't use sub-second timing.
func (*testHooks) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return 0
}

// EndOfBlockOps returns nil as ethereum tests don't have end-of-block operations.
func (*testHooks) EndOfBlockOps(*types.Block) []hook.Op {
	return nil
}

// BeforeExecutingBlock processes the beacon block root if present (block tests only).
func (t *testHooks) BeforeExecutingBlock(_ params.Rules, statedb *state.StateDB, b *types.Block) error {
	if t.consensus == nil {
		return nil
	}
	// Block test: process beacon root if present
	if beaconRoot := b.BeaconRoot(); beaconRoot != nil {
		chainContext := &chainContext{engine: t.consensus, readerAdapter: t.reader}
		context := core.NewEVMBlockContext(b.Header(), chainContext, nil)
		vmenv := vm.NewEVM(context, vm.TxContext{}, statedb, chainContext.Config(), vm.Config{})
		core.ProcessBeaconBlockRoot(*beaconRoot, vmenv, statedb)
	}
	return nil
}

// AfterExecutingBlock finalizes the block.
// For block tests: updates total difficulty and calls consensus finalization.
// For state tests: touches the coinbase address to ensure it exists.
func (t *testHooks) AfterExecutingBlock(statedb *state.StateDB, b *types.Block, receipts types.Receipts) {
	if t.consensus != nil {
		// Block test: update total difficulty and finalize
		currentNumber := b.NumberU64()
		currentTd := big.NewInt(0)
		if currentNumber > 0 {
			currentTd = t.reader.GetTd(b.ParentHash(), currentNumber-1)
			if currentTd == nil {
				currentTd = big.NewInt(0)
				if t.reader.logger != nil {
					t.reader.logger.Error("currentTd is nil")
				}
			}
		}
		newTd := new(big.Int).Add(currentTd, b.Difficulty())
		t.reader.SetTd(b.Hash(), b.NumberU64(), newTd.Uint64())
		t.consensus.Finalize(t.reader, b.Header(), statedb, b.Transactions(), b.Uncles(), b.Withdrawals())
		return
	}

	// State test: Touch coinbase with 0-value mining reward.
	// This makes a difference when:
	// - the coinbase self-destructed, or
	// - there are only 'bad' transactions which aren't executed, so the
	//   coinbase gets no txfee and isn't created, thus needs to be touched
	statedb.AddBalance(b.Coinbase(), new(uint256.Int))
}

// chainContext adapts testHooks to implement core.ChainContext for EVM creation.
type chainContext struct {
	engine consensus.Engine
	*readerAdapter
}

var _ core.ChainContext = (*chainContext)(nil)

func (c *chainContext) Engine() consensus.Engine {
	return c.engine
}
