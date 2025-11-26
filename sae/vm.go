// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
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
	hooks   hook.Points
	exec    *saexec.Executor
	db      ethdb.Database
	toClose [](func() error)
	mempool *txgossip.Set
	snowCtx *snow.Context
	now     func() time.Time

	snowState  utils.Atomic[snow.State]
	preference atomic.Pointer[blocks.Block]
	last       last
	blocks     sMap[common.Hash, *blocks.Block]
}

type last struct {
	synchronous                 struct{ height, time uint64 }
	accepted, executed, settled atomic.Pointer[blocks.Block]
}

func NewVM() *VM {
	return &VM{
		now: time.Now,
	}
}

func (vm *VM) Init(
	snowCtx *snow.Context,
	lastSynchronous *blocks.Block,
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	triedbConfig *triedb.Config,
	hooks hook.Points,
) error {
	vm.hooks = hooks
	vm.snowCtx = snowCtx
	vm.preference.Store(lastSynchronous)
	vm.last.accepted.Store(lastSynchronous)
	vm.blocks.Store(lastSynchronous.Hash(), lastSynchronous)
	vm.db = db

	exec, err := saexec.New(
		lastSynchronous,
		vm.blockSource,
		chainConfig,
		db,
		triedbConfig,
		hooks,
		snowCtx.Log,
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
	if err := snowCtx.Metrics.Register("SAE", metrics); err != nil {
		return err
	}
	bloom, err := gossip.NewBloomFilter(metrics, "txgossip_bloom_filter", 1e3, 1e-2, 1e-2)
	if err != nil {
		return err
	}

	vm.exec = exec
	vm.mempool = txgossip.NewSet(snowCtx.Log, txPool, bloom)
	vm.toClose = append(vm.toClose, txPool.Close, exec.Close)

	return nil
}

// SetState notifies the VM of a transition in the state lifecycle.
func (vm *VM) SetState(ctx context.Context, state snow.State) error {
	vm.snowState.Set(state)
	return nil
}

// Shutdown gracefully closes the VM.
func (vm *VM) Shutdown(context.Context) error {
	errs := make([]error, len(vm.toClose))
	for i, fn := range vm.toClose {
		errs[i] = fn()
	}
	return errors.Join(errs...)
}

// Version reports the VM's version.
func (vm *VM) Version(context.Context) (string, error) {
	return "", errUnimplemented
}

func (vm *VM) log() logging.Logger {
	return vm.snowCtx.Log
}

func (vm *VM) rulesForBlock(b *types.Block) params.Rules {
	return vm.exec.ChainConfig().Rules(b.Number(), true /*isMerge*/, b.Time())
}

func (vm *VM) signerForBlock(b *types.Block) types.Signer {
	return types.MakeSigner(vm.exec.ChainConfig(), b.Number(), b.Time())
}
