// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hookstest provides a test double for SAE's [hook] package.
package hookstest

import (
	"context"

	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saetest"
)

// Stub implements [hook.Points].
type Stub struct {
	Target        gas.Gas
	SubSecondTime gas.Gas
	Ops           []hook.Op
}

var _ hook.Points = (*Stub)(nil)

// BlockEvents immediately returns nil.
func (*Stub) BlockEvents(context.Context) error {
	return nil
}

// WaitForEvent blocks until the context is cancelled, and then returns the
// context's error.
func (*Stub) WaitForEvent(ctx context.Context) (common.Message, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// BuildBlock calls [types.NewBlock] with its arguments.
func (*Stub) BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) *types.Block {
	return types.NewBlock(header, txs, nil, receipts, saetest.TrieHasher())
}

// BlockRebuilderFrom ignores its argument and returns itself.
func (s *Stub) BlockRebuilderFrom(*types.Block) hook.BlockBuilder {
	return s
}

// GasTargetAfter ignores its argument and always returns [Stub.Target].
func (s *Stub) GasTargetAfter(*types.Header) gas.Gas {
	return s.Target
}

// SubSecondBlockTime time ignores its arguments and always returns
// [Stub.SubSecondTime].
func (s *Stub) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return s.SubSecondTime
}

// EndOfBlockOps ignores its argument and always returns [Stub.Ops].
func (s *Stub) EndOfBlockOps(*types.Block) []hook.Op {
	return s.Ops
}

// BeforeExecutingBlock is a no-op that always returns nil.
func (*Stub) BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error {
	return nil
}

// AfterExecutingBlock is a no-op.
func (*Stub) AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts) {}
