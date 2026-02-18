// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"crypto/rand"
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
	if bCtx == nil {
		bCtx := new(vm.BlockContext)
		*bCtx = core.NewEVMBlockContext(hdr, b.vm.exec.ChainContext(), randomAddress())
	}
	txCtx := core.NewEVMTxContext(msg)
	return vm.NewEVM(*bCtx, txCtx, sdb, b.ChainConfig(), *cfg)
}

// randomAddress returns a randomly generated address for use as a fake
// [types.Header.Coinbase] instead of hard-coding a value that can be detected
// by malicious contracts that then modify their behaviour during simulation.
func randomAddress() *common.Address {
	var a common.Address
	rand.Read(a[:]) //nolint:gosec,errcheck // Documented as never returning an error
	return &a
}

// StateAndHeaderByNumber performs the same faking as
// [ethAPIBackend.StateAndHeaderByNumberOrHash].
func (b *ethAPIBackend) StateAndHeaderByNumber(ctx context.Context, num rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	return b.StateAndHeaderByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(num))
}

// StateAndHeaderByNumberOrHash fakes the returned [types.Header] to contain
// post-execution results, mimicking a synchronous block. The [state.StateDB] is
// opened at the post-execution root, as carried by the faked header.
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
