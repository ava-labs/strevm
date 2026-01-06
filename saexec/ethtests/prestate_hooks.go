// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/hook"
)

// preStateHooks implements [hook.Points] with pre-state application support.
type preStateHooks struct {
	preState types.GenesisAlloc // Pre-state to apply for genesis block
}

var _ hook.Points = (*preStateHooks)(nil)

func (p *preStateHooks) BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) *types.Block {
	return nil
}

func (p *preStateHooks) BuildHeader(parent *types.Header) *types.Header {
	return nil
}

func newPreStateHooks(preState types.GenesisAlloc) *preStateHooks {
	return &preStateHooks{preState: preState}
}

// BlockRebuilderFrom ignores its argument and always returns itself.
func (p *preStateHooks) BlockRebuilderFrom(block *types.Block) hook.BlockBuilder {
	return p
}

// GasTargetAfter ignores its argument and always returns [preStateHooks.target].
func (p *preStateHooks) GasTargetAfter(*types.Header) gas.Gas {
	return 1e6
}

// SubSecondBlockTime ignores its argument and always returns 0.
func (*preStateHooks) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return 0
}

// EndOfBlockOps is a no-op.
func (*preStateHooks) EndOfBlockOps(*types.Block) []hook.Op {
	return nil
}

// BeforeExecutingBlock applies the pre-state allocation if this is the genesis block,
// then processes the beacon block root if present.
func (p *preStateHooks) BeforeExecutingBlock(r params.Rules, statedb *state.StateDB, b *types.Block) error {
	// // Apply pre-state allocation
	// if p.preState != nil {
	// 	for addr, account := range p.preState {
	// 		statedb.SetCode(addr, account.Code)
	// 		statedb.SetNonce(addr, account.Nonce)
	// 		statedb.SetBalance(addr, uint256.MustFromBig(account.Balance))
	// 		for k, v := range account.Storage {
	// 			statedb.SetState(addr, k, v)
	// 		}
	// 	}
	// }
	// // statedb.Commit(0, false)
	return nil
}

// AfterExecutingBlock finalizes the block and updates the total difficulty.
func (p *preStateHooks) AfterExecutingBlock(statedb *state.StateDB, b *types.Block, receipts types.Receipts) {
	// Add 0-value mining reward. This only makes a difference in the cases
	// where
	// - the coinbase self-destructed, or
	// - there are only 'bad' transactions, which aren't executed. In those cases,
	//   the coinbase gets no txfee, so isn't created, and thus needs to be touched
	statedb.AddBalance(b.Coinbase(), new(uint256.Int))
}
