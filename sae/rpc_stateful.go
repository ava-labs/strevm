// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/blocks"
)

func (b *ethAPIBackend) RPCEVMTimeout() time.Duration {
	return b.vm.config.RPCConfig.EVMTimeout
}

func (b *ethAPIBackend) RPCGasCap() uint64 {
	return b.vm.config.RPCConfig.GasCap
}

func (b *ethAPIBackend) Engine() consensus.Engine {
	return (*coinbaseAsAuthor)(nil)
}

type coinbaseAsAuthor struct {
	consensus.Engine
}

func (*coinbaseAsAuthor) Author(h *types.Header) (common.Address, error) {
	return h.Coinbase, nil
}

func (b *ethAPIBackend) GetEVM(ctx context.Context, msg *core.Message, sdb *state.StateDB, hdr *types.Header, cfg *vm.Config, bCtx *vm.BlockContext) *vm.EVM {
	txCtx := vm.TxContext{
		Origin:   msg.From,
		GasPrice: big.NewInt(1),
	}
	return vm.NewEVM(*bCtx, txCtx, sdb, b.ChainConfig(), *cfg)
}

func (b *ethAPIBackend) StateAndHeaderByNumber(ctx context.Context, num rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	return b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(num))
}

func (b *ethAPIBackend) StateAndHeaderByNumberOrHash(ctx context.Context, numOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	if n, ok := numOrHash.Number(); ok && n == rpc.PendingBlockNumber {
		return nil, nil, errors.New("state not available for pending block")
	}

	num, hash, err := b.resolveBlockNumberOrHash(numOrHash)
	if err != nil {
		return nil, nil, err
	}

	if bl, ok := b.vm.blocks.Load(hash); ok {
		sdb, err := state.New(bl.PostExecutionStateRoot(), b.exec.StateCache(), nil)
		if err != nil {
			return nil, nil, err
		}
		return sdb, types.CopyHeader(bl.Header()), nil
	}

	root, err := blocks.PostExecutionStateRoot(b.vm.xdb, num)
	if err != nil {
		return nil, nil, err
	}
	sdb, err := state.New(root, b.exec.StateCache(), nil)
	if err != nil {
		return nil, nil, err
	}
	return sdb, rawdb.ReadHeader(b.db, hash, num), nil
}
