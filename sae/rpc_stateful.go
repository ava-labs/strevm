// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
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
func (b *ethAPIBackend) StateAtBlock(_ context.Context, block *types.Block, _ uint64, _ *state.StateDB, _ bool, _ bool) (*state.StateDB, tracers.StateReleaseFunc, error) {
	hash := block.Hash()
	num := block.NumberU64()

	var root common.Hash
	if bl, ok := b.vm.blocks.Load(hash); ok {
		if !bl.Executed() {
			return nil, nil, fmt.Errorf("execution results not yet available for block %d", num)
		}
		root = bl.PostExecutionStateRoot()
	} else {
		var err error
		root, err = blocks.PostExecutionStateRoot(b.vm.xdb, num)
		if err != nil {
			return nil, nil, err
		}
	}

	sdb, err := state.New(root, b.exec.StateCache(), nil)
	if err != nil {
		return nil, nil, err
	}
	return sdb, func() {}, nil
}

// StateAtTransaction returns the execution environment of a particular
// transaction within a block. It replays all preceding transactions to produce
// the state just before the target transaction, then returns the message and
// block context needed for tracing.
//
// NOTE: The replay follows the same flow as [saexec.Executor.execute]: parent state,
// BeforeExecutingBlock hook, execution base fee, then transaction application.
// It omits execution-only concerns (gas clock, receipts, bound checks,
// EndOfBlockOps, AfterExecutingBlock) that are irrelevant to state tracing.
func (b *ethAPIBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, _ uint64) (*core.Message, vm.BlockContext, *state.StateDB, tracers.StateReleaseFunc, error) {
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

	stateDB, release, err := b.StateAtBlock(ctx, parentEthBlock, 0, nil, false, false)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}
	defer release()

	// Replicate the execution pipeline ordering from saexec/execution.go:
	// hooks run before the header is modified with the execution base fee.
	chainConfig := b.ChainConfig()
	rules := chainConfig.Rules(block.Number(), true /*isMerge*/, block.Time())
	if err := b.vm.exec.Hooks().BeforeExecutingBlock(rules, stateDB, block); err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}

	// Replace the worst-case consensus base fee in the header with the
	// actual execution base fee, as done during block execution.
	header := types.CopyHeader(block.Header())
	if bl, ok := b.vm.blocks.Load(block.Hash()); ok {
		header.BaseFee = bl.BaseFee().ToBig()
	} else {
		bf, err := blocks.ExecutionBaseFee(b.vm.xdb, block.NumberU64())
		if err != nil {
			return nil, vm.BlockContext{}, nil, nil, err
		}
		header.BaseFee = bf.ToBig()
	}

	signer := types.MakeSigner(chainConfig, block.Number(), block.Time())
	blockCtx := core.NewEVMBlockContext(header, b.vm.exec.ChainContext(), &header.Coinbase)
	gasPool := core.GasPool(math.MaxUint64)

	// Replay transactions 0..txIndex-1 to produce the state just before the
	// target transaction.
	for i, tx := range txs[:txIndex] {
		stateDB.SetTxContext(tx.Hash(), i)
		msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, vm.BlockContext{}, nil, nil, err
		}
		txCtx := core.NewEVMTxContext(msg)
		evm := vm.NewEVM(blockCtx, txCtx, stateDB, chainConfig, vm.Config{})
		if _, err := core.ApplyMessage(evm, msg, &gasPool); err != nil {
			return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("replaying tx %d: %v", i, err)
		}
		stateDB.Finalise(true) // always true for post-merge
	}

	// Prepare the target transaction's message.
	targetTx := txs[txIndex]
	msg, err := core.TransactionToMessage(targetTx, signer, header.BaseFee)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, err
	}
	return msg, blockCtx, stateDB, func() {}, nil
}
