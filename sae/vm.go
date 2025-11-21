// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package sae implements the [Streaming Asynchronous Execution] (SAE) virtual
// machine to be compatible with Avalanche consensus.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package sae

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/txgossip"
)

// VM implements all of [adaptor.ChainVM] except for the `Initialize` method,
// which needs to be provided by a harness. In all cases, the harness MUST
// provide a last-synchronous block, which MAY be the genesis.
type VM struct {
	exec       *saexec.Executor
	toClose    [](func() error)
	mempool    *txgossip.Set
	log        logging.Logger
	now        func() time.Time
	preference atomic.Pointer[blocks.Block]
}

func NewVM() *VM {
	return &VM{
		now: time.Now,
	}
}

func (vm *VM) Init(
	chainCtx *snow.Context,
	lastSynchronous *blocks.Block,
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	triedbConfig *triedb.Config,
	hooks hook.Points,
) error {
	exec, err := saexec.New(
		lastSynchronous,
		vm.blockSource,
		chainConfig,
		db,
		triedbConfig,
		hooks,
		chainCtx.Log,
	)
	if err != nil {
		return fmt.Errorf("saexec.New(...): %v", err)
	}

	bc := txgossip.NewBlockChain(exec, vm.blockSource)
	pools := []txpool.SubPool{
		legacypool.New(legacypool.DefaultConfig, bc),
	}
	txPool, err := txpool.New(0, bc, pools)
	if err != nil {
		return fmt.Errorf("txpool.New(...): %v", err)
	}

	metrics := prometheus.NewRegistry()
	if err := chainCtx.Metrics.Register("SAE", metrics); err != nil {
		return err
	}
	bloom, err := gossip.NewBloomFilter(metrics, "txgossip_bloom_filter", 1e3, 1e-2, 1e-2)
	if err != nil {
		return err
	}

	vm.exec = exec
	vm.mempool = txgossip.NewSet(chainCtx.Log, txPool, bloom)
	vm.toClose = append(vm.toClose, txPool.Close, vm.mempool.Close)

	return nil
}

// SetState notifies the VM of a transition in the state lifecycle.
func (vm *VM) SetState(ctx context.Context, state snow.State) error {
	return errUnimplemented
}

// Shutdown gracefully closes the VM.
func (vm *VM) Shutdown(context.Context) error {
	return errUnimplemented
}

// Version reports the VM's version.
func (vm *VM) Version(context.Context) (string, error) {
	return "", errUnimplemented
}
