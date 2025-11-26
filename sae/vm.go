// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/snow"
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
	snowCtx *snow.Context
	hooks   hook.Points
	now     func() time.Time

	blocks     sMap[common.Hash, *blocks.Block]
	preference atomic.Pointer[blocks.Block]

	exec    *saexec.Executor
	mempool *txgossip.Set

	toClose [](func() error)
}

func NewVM() *VM {
	return &VM{
		now: time.Now,
	}
}

func (vm *VM) Init(
	snowCtx *snow.Context,
	hooks hook.Points,
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	triedbConfig *triedb.Config,
	lastSynchronous *blocks.Block,
) error {
	vm.snowCtx = snowCtx
	vm.hooks = hooks

	vm.blocks.Store(lastSynchronous.Hash(), lastSynchronous)
	vm.preference.Store(lastSynchronous)

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
	return errUnimplemented
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
	return vm.rulesAt(b.Number(), b.Time())
}

func (vm *VM) rulesAt(height *big.Int, time uint64) params.Rules {
	return vm.exec.ChainConfig().Rules(height, true /*isMerge*/, time)
}

func (vm *VM) signerForBlock(b *types.Block) types.Signer {
	return types.MakeSigner(vm.exec.ChainConfig(), b.Number(), b.Time())
}
