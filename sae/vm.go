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
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/bloom"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
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
	config  Config
	snowCtx *snow.Context
	hooks   hook.Points
	now     func() time.Time
	metrics *prometheus.Registry

	blocks     sMap[common.Hash, *blocks.Block]
	preference atomic.Pointer[blocks.Block]

	exec    *saexec.Executor
	mempool *txgossip.Set
	newTxs  chan struct{}

	toClose [](func() error)
}

// A Config configures construction of a new [VM].
type Config struct {
	MempoolConfig legacypool.Config

	Now func() time.Time // defaults to [time.Now] if nil
}

// NewVM returns a new [VM] on which the [VM.Init] method MUST be called before
// other use. This deferment allows the [VM] to be constructed before
// `Initialize` has been called on the harness.
func NewVM(c Config) *VM {
	now := c.Now
	if now == nil {
		now = time.Now
	}

	return &VM{
		config: Config{},
		now:    now,
	}
}

// Init initializes the [VM], similarly to the `Initialize` method of
// [snowcommon.VM].
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

	vm.metrics = prometheus.NewRegistry()
	if err := snowCtx.Metrics.Register("SAE", vm.metrics); err != nil {
		return err
	}

	{ // ==========  Executor  ==========
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
		vm.exec = exec
		vm.toClose = append(vm.toClose, exec.Close)
	}

	{ // ==========  Mempool  ==========
		bc := txgossip.NewBlockChain(vm.exec, vm.blockSource)
		pools := []txpool.SubPool{
			legacypool.New(vm.config.MempoolConfig, bc),
		}
		txPool, err := txpool.New(0, bc, pools)
		if err != nil {
			return fmt.Errorf("txpool.New(...): %v", err)
		}
		vm.toClose = append(vm.toClose, txPool.Close)

		metrics, err := bloom.NewMetrics("mempool", vm.metrics)
		if err != nil {
			return err
		}
		pool, err := txgossip.NewSet(snowCtx.Log, txPool, gossip.BloomSetConfig{
			Metrics:                        metrics,
			MinTargetElements:              10,
			TargetFalsePositiveProbability: 0.1,
			ResetFalsePositiveProbability:  0.1,
		})
		if err != nil {
			return err
		}
		vm.mempool = pool
		vm.signalNewTxsToEngine()
	}

	return nil
}

// signalNewTxsToEngine subscribes to the [txpool.TxPool] to unblock a current
// or future call to [VM.WaitForEvent]. At most one future call will be
// unblocked through the use of a buffered channel. [VM.Shutdown] MUST be
// called to release a goroutine started by this method.
func (vm *VM) signalNewTxsToEngine() {
	ch := make(chan core.NewTxsEvent)
	sub := vm.mempool.Pool.SubscribeTransactions(ch, false /*reorgs but ignored by legacypool*/)
	vm.toClose = append(vm.toClose, func() error {
		defer close(ch)
		sub.Unsubscribe()
		return <-sub.Err() // guaranteed to be closed due to unsubscribing
	})

	vm.newTxs = make(chan struct{}, 1)
	go func() {
		defer close(vm.newTxs)
		for range ch {
			select {
			case vm.newTxs <- struct{}{}:
				_ = 0 // coverage visualisation
			default:
				_ = 0 // coverage visualization
			}
		}
	}()
}

// WaitForEvent blocks until the VM needs to signal the consensus engine, and
// then returns the message to send.
func (vm *VM) WaitForEvent(ctx context.Context) (snowcommon.Message, error) {
	select {
	case _, ok := <-vm.newTxs:
		if !ok {
			return 0, errors.New("VM closed")
		}
		return snowcommon.PendingTxs, nil

	case <-ctx.Done():
		return 0, context.Cause(ctx)
	}
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

func (vm *VM) signerForBlock(b *types.Block) types.Signer {
	return types.MakeSigner(vm.exec.ChainConfig(), b.Number(), b.Time())
}
