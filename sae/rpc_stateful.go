// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
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
	txCtx := core.NewEVMTxContext(msg)
	// TODO(arr4n) eth_accessList provides a nil [vm.BlockContext] so it will
	// need to be constructed.
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

	// The API implementations expect this to be synchronous, sourcing the state
	// root and the base fee from fields. At the time of writing, the returned
	// header's hash is never used so it's safe to modify it.
	//
	// TODO(arr4n) the above assumption is brittle under geth/libevm updates;
	// devise an approach to ensure that it is confirmed on each.
	var hdr *types.Header

	if bl, ok := b.vm.blocks.Load(hash); ok {
		hdr = bl.Header()
		hdr.Root = bl.PostExecutionStateRoot()
		hdr.BaseFee = bl.BaseFee().ToBig()
	} else {
		hdr = rawdb.ReadHeader(b.db, hash, num)

		// TODO(arr4n) export [blocks.executionResults] to avoid multiple
		// database reads and canoto unmarshallings here.
		var err error
		hdr.Root, err = blocks.PostExecutionStateRoot(b.vm.xdb, num)
		if err != nil {
			return nil, nil, err
		}

		bf, err := blocks.ExecutionBaseFee(b.vm.xdb, num)
		if err != nil {
			return nil, nil, err
		}
		hdr.BaseFee = bf.ToBig()
	}

	sdb, err := state.New(hdr.Root, b.exec.StateCache(), nil)
	if err != nil {
		return nil, nil, err
	}
	return sdb, hdr, nil
}
