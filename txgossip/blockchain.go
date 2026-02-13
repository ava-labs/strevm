// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

// A BlockChain is the union of [txpool.BlockChain] and [legacypool.BlockChain].
type BlockChain interface {
	txpool.BlockChain
	legacypool.BlockChain
}

// NewBlockChain wraps an [saexec.Executor] to be compatible with a
// non-blob-transaction mempool. The returned instance assumes that the
// consuming mempool expects a synchronous chain so its `StateAt()` method
// always returns the last executed block's post-execution state.
func NewBlockChain(exec *saexec.Executor, blocks blocks.Source) BlockChain {
	return &blockchain{
		exec:   exec,
		blocks: blocks,
	}
}

type blockchain struct {
	exec   *saexec.Executor
	blocks blocks.Source
}

func (bc *blockchain) Config() *params.ChainConfig {
	return bc.exec.ChainConfig()
}

func (bc *blockchain) CurrentBlock() *types.Header {
	return bc.exec.LastExecuted().Header()
}

func (bc *blockchain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return bc.blocks.EthBlock(hash, number)
}

// StateAt is a filthy liar! Instead of opening the requested root, it assumes
// that it is a settled root and instead opens the latest known post-execution
// state root of a block that has the requested root as its last-settled state.
func (bc *blockchain) StateAt(common.Hash) (*state.StateDB, error) {
	root := bc.exec.LastExecuted().PostExecutionStateRoot()
	return state.New(root, bc.exec.StateCache(), nil)
}

func (bc *blockchain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return bc.exec.SubscribeChainHeadEvent(ch)
}
