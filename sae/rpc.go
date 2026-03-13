// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	saerpc "github.com/ava-labs/strevm/sae/rpc"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/txgossip"
)

// GethRPCBackends returns the backing infrastructure for geth's implementations
// of the JSON-RPC namespaces supported by the VM.
func (vm *VM) GethRPCBackends() saerpc.GethBackends {
	return vm.rpcProvider.GethBackends()
}

func (vm *VM) rpcChain() saerpc.Chain {
	return rpcChain{vm, vm.exec}
}

type rpcChain struct {
	*VM
	*saexec.Executor
}

func (c rpcChain) Logger() logging.Logger      { return c.VM.snowCtx.Log }
func (c rpcChain) Hooks() hook.Points          { return c.hooks }
func (c rpcChain) DB() ethdb.Database          { return c.db }
func (c rpcChain) XDB() saedb.ExecutionResults { return c.xdb }
func (c rpcChain) Mempool() *txgossip.Set      { return c.mempool }
func (c rpcChain) Peers() *p2p.Peers           { return c.VM.Peers }
func (c rpcChain) LastAccepted() *blocks.Block { return c.last.accepted.Load() }
func (c rpcChain) LastSettled() *blocks.Block  { return c.last.settled.Load() }

func (c rpcChain) ConsensusCriticalBlock(h common.Hash) (*blocks.Block, bool) {
	return c.consensusCritical.Load(h)
}

func (c rpcChain) NewBlock(eth *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	return c.blockBuilder.new(eth, parent, lastSettled)
}

func (c rpcChain) SubscribeAcceptedBlocks(ch chan<- *blocks.Block) event.Subscription {
	return c.acceptedBlocks.Subscribe(ch)
}
