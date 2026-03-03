// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/eth/tracers"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

var noopRelease tracers.StateReleaseFunc = func() {}

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
		bCtx = new(vm.BlockContext)
		*bCtx = core.NewEVMBlockContext(hdr, b.vm.exec.ChainContext(), &hdr.Coinbase)
	}
	txCtx := core.NewEVMTxContext(msg)
	return vm.NewEVM(*bCtx, txCtx, sdb, b.ChainConfig(), *cfg)
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
		hdr.Root, err = b.vm.postExecutionStateRoot(hash, num)
		if err != nil {
			return nil, nil, err
		}
		hdr.BaseFee, err = b.vm.executionBaseFee(hash, num)
		if err != nil {
			return nil, nil, err
		}
	}

	sdb, err := state.New(hdr.Root, b.exec.StateCache(), nil)
	if err != nil {
		return nil, nil, err
	}
	return sdb, hdr, nil
}

// StateAtBlock returns the state database after executing the given block. The
// reexec, base, readOnly, and preferDisk parameters are ignored because SAE
// does not implement geth's re-execution-from-archive strategy.
//
// Like geth, SAE only stores historical state roots, not full historical state.
// The underlying trie data must still be present in the state cache/DB for
// [state.New] to succeed. This means tracing is limited to recent blocks whose
// trie data has not been pruned (or requires an archival node for older blocks).
//
// Reference: https://geth.ethereum.org/docs/developers/evm-tracing#state-availability
func (b *ethAPIBackend) StateAtBlock(_ context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, tracers.StateReleaseFunc, error) {
	// Guard against in-memory blocks that haven't been executed yet.
	if bl, ok := b.vm.blocks.Load(block.Hash()); ok && !bl.Executed() {
		return nil, nil, fmt.Errorf("execution results not yet available for block %d", block.NumberU64())
	}

	root, err := b.vm.postExecutionStateRoot(block.Hash(), block.NumberU64())
	if err != nil {
		return nil, nil, err
	}

	sdb, err := state.New(root, b.exec.StateCache(), nil)
	if err != nil {
		return nil, nil, err
	}
	return sdb, noopRelease, nil
}

// StateAtTransaction returns the execution environment of a particular
// transaction within a block. It replays all preceding transactions to produce
// the state just before the target transaction, then returns the message and
// block context needed for tracing.
//
// Replay uses [saexec.NewBlockExecution] and [saexec.BlockExecution.ExecuteBlock] to
// guarantee identical pipeline ordering with [saexec.Executor.execute].
// Execution-only concerns (gas clock, bound checks) are omitted via nil
// callbacks. End-of-block operations are suppressed by passing nil Hooks.
//
// The returned release function calls
// [saexec.BlockExecution.AfterExecutingBlock] to clean up any resources
// created by [hook.Points.BeforeExecutingBlock].
func (b *ethAPIBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (*core.Message, vm.BlockContext, *state.StateDB, tracers.StateReleaseFunc, error) {
	if block.NumberU64() == 0 {
		return nil, vm.BlockContext{}, nil, nil, errors.New("no transactions in genesis")
	}
	txs := block.Transactions()
	if txIndex < 0 || txIndex >= len(txs) {
		return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("transaction index %d out of range [0, %d)", txIndex, len(txs))
	}

	// Look up the parent block to obtain its post-execution state.
	var parentEthBlock *types.Block
	if parentBl, ok := b.vm.blocks.Load(block.ParentHash()); ok {
		parentEthBlock = parentBl.EthBlock()
	} else {
		parentEthBlock = rawdb.ReadBlock(b.db, block.ParentHash(), block.NumberU64()-1)
	}
	if parentEthBlock == nil {
		return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("parent block %#x not found", block.ParentHash())
	}

	stateDB, _, err := b.StateAtBlock(ctx, parentEthBlock, 0, nil, false, false)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}

	baseFee, err := b.vm.executionBaseFee(block.Hash(), block.NumberU64())
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}

	prep, err := saexec.NewBlockExecution(stateDB, block, baseFee, b.vm.hooks, b.ChainConfig(), b.vm.exec.ChainContext())
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}

	// Replay transactions 0..txIndex-1 to produce the state just before the
	// target transaction.
	if _, _, err := prep.ExecuteBlock(&saexec.ExecuteBlockConfig{
		Transactions: txs[:txIndex],
	}); err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}

	// Prepare the target transaction's message.
	targetTx := txs[txIndex]
	msg, err := core.TransactionToMessage(targetTx, prep.Signer, prep.Header.BaseFee)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}
	release := func() {
		prep.AfterExecutingBlock(nil)
	}
	return msg, prep.BlockCtx, stateDB, release, nil
}

// postExecutionStateRoot returns the post-execution state root for the block
// identified by hash and number, checking in-memory blocks first, then falling
// back to disk.
func (vm *VM) postExecutionStateRoot(hash common.Hash, num uint64) (common.Hash, error) {
	if bl, ok := vm.blocks.Load(hash); ok {
		return bl.PostExecutionStateRoot(), nil
	}
	return blocks.PostExecutionStateRoot(vm.xdb, num)
}

// executionBaseFee returns the execution base fee for the block identified by
// hash and number, checking in-memory blocks first, then falling back to disk.
func (vm *VM) executionBaseFee(hash common.Hash, num uint64) (*big.Int, error) {
	if bl, ok := vm.blocks.Load(hash); ok {
		return bl.BaseFee().ToBig(), nil
	}
	bf, err := blocks.ExecutionBaseFee(vm.xdb, num)
	if err != nil {
		return nil, err
	}
	return bf.ToBig(), nil
}
