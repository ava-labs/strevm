// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/bloom"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
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
	*p2p.Network
	peers *p2p.Peers

	config  Config
	snowCtx *snow.Context
	hooks   hook.Points
	metrics *prometheus.Registry

	db     ethdb.Database
	blocks *sMap[common.Hash, *blocks.Block]

	consensusState utils.Atomic[snow.State]
	preference     atomic.Pointer[blocks.Block]
	last           struct {
		accepted, settled atomic.Pointer[blocks.Block]
	}

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
	if c.Now == nil {
		c.Now = time.Now
	}
	return &VM{
		config: c,
		blocks: newSMap[common.Hash, *blocks.Block](),
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
	sender snowcommon.AppSender,
) error {
	vm.snowCtx = snowCtx
	vm.hooks = hooks
	vm.db = db
	vm.blocks.Store(lastSynchronous.Hash(), lastSynchronous)

	// Disk
	for _, fn := range [](func(ethdb.KeyValueWriter, common.Hash)){
		rawdb.WriteHeadBlockHash,
		rawdb.WriteFinalizedBlockHash,
	} {
		fn(db, lastSynchronous.Hash())
	}
	// Internal indicators
	for _, ptr := range []*atomic.Pointer[blocks.Block]{
		&vm.preference,
		&vm.last.accepted,
		&vm.last.settled,
	} {
		ptr.Store(lastSynchronous)
	}

	vm.metrics = prometheus.NewRegistry()
	if err := snowCtx.Metrics.Register("sae", vm.metrics); err != nil {
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
		conf := gossip.BloomSetConfig{Metrics: metrics}
		pool, err := txgossip.NewSet(snowCtx.Log, txPool, conf)
		if err != nil {
			return err
		}
		vm.mempool = pool
		vm.signalNewTxsToEngine()
	}

	{ // ==========  P2P Gossip  ==========
		network, peers, validatorPeers, err := newNetwork(snowCtx, sender, vm.metrics)
		if err != nil {
			return fmt.Errorf("newNetwork(...): %v", err)
		}

		systemConfig := gossip.SystemConfig{
			Log:       snowCtx.Log,
			Registry:  vm.metrics,
			Namespace: "gossip",
		}
		systemConfig.SetDefaults()
		handler, pullGossiper, pushGossiper, err := gossip.NewSystem(
			snowCtx.NodeID,
			network,
			validatorPeers,
			vm.mempool,
			txgossip.Marshaller{},
			systemConfig,
		)
		if err != nil {
			return fmt.Errorf("gossip.NewSystem(...): %v", err)
		}
		if err := network.AddHandler(systemConfig.HandlerID, handler); err != nil {
			return fmt.Errorf("network.AddHandler(...): %v", err)
		}

		var (
			gossipCtx, cancel = context.WithCancel(context.Background())
			wg                sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			gossip.Every(gossipCtx, snowCtx.Log, pullGossiper, systemConfig.RequestPeriod)
		}()
		go func() {
			defer wg.Done()
			const pushGossipPeriod = 100 * time.Millisecond
			gossip.Every(gossipCtx, snowCtx.Log, pushGossiper, pushGossipPeriod)
		}()

		vm.Network = network
		vm.peers = peers
		vm.mempool.RegisterPushGossiper(pushGossiper)
		vm.toClose = append(vm.toClose, func() error {
			cancel()
			wg.Wait()
			return nil
		})
	}

	return nil
}

// signalNewTxsToEngine subscribes to the [txpool.TxPool] to unblock
// [VM.WaitForEvent] when necessary. [VM.Shutdown] MUST be called to release a
// goroutine started by this method.
func (vm *VM) signalNewTxsToEngine() {
	ch := make(chan core.NewTxsEvent)
	sub := vm.mempool.Pool.SubscribeTransactions(ch, false /*reorgs but ignored by legacypool*/)
	vm.toClose = append(vm.toClose, func() error {
		defer close(ch)
		sub.Unsubscribe()
		return <-sub.Err() // guaranteed to be closed due to unsubscribing
	})

	// See [VM.WaitForEvent] for why this requires a buffer.
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

// WaitForEvent returns immediately if there are already pending transactions in
// the mempool, otherwise it blocks until the mempool notifies it of new
// transactions. In both cases it returns [snowcommon.PendingTxs]. In the latter
// scenario it respects context cancellation.
func (vm *VM) WaitForEvent(ctx context.Context) (snowcommon.Message, error) {
	if vm.numPendingTxs() > 0 {
		select {
		case <-vm.newTxs: // probably has something buffered
		default:
		}
		return snowcommon.PendingTxs, nil
	}

	// Sends on the `newTxs` channel are performed on a best-effort basis, which
	// could race here if it weren't for the channel buffer.

	for {
		select {
		case _, ok := <-vm.newTxs:
			if !ok {
				return 0, errors.New("VM closed")
			}
			if vm.numPendingTxs() > 0 {
				return snowcommon.PendingTxs, nil
			}

		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
	}
}

func (vm *VM) numPendingTxs() int {
	p, _ := vm.mempool.Pool.Stats()
	return p
}

// SetState notifies the VM of a transition in the state lifecycle.
func (vm *VM) SetState(ctx context.Context, state snow.State) error {
	vm.consensusState.Set(state)
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

func (vm *VM) signerForBlock(b *types.Block) types.Signer {
	return types.MakeSigner(vm.exec.ChainConfig(), b.Number(), b.Time())
}
