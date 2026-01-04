// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/txgossip"
)

func (vm *VM) ethRPCServer() (*rpc.Server, error) {
	b := &ethAPIBackend{
		vm:  vm,
		Set: vm.mempool,
	}
	s := rpc.NewServer()

	// Even if this function errors, we should close API to prevent a goroutine
	// from leaking.
	filterSystem := filters.NewFilterSystem(b, filters.Config{})
	filterAPI := filters.NewFilterAPI(filterSystem, false /*isLightClient*/)
	vm.toClose = append(vm.toClose, func() error {
		filterAPI.Close()
		return nil
	})

	// Standard Ethereum APIs are documented at: https://ethereum.org/developers/docs/apis/json-rpc
	// Geth-specific APIs are documented at: https://geth.ethereum.org/docs/interacting-with-geth/rpc
	apis := []struct {
		namespace string
		api       any
	}{
		// Standard Ethereum node APIs:
		// - web3_clientVersion
		// - web3_sha3
		{"web3", newWeb3API()},

		{"eth", ethapi.NewBlockChainAPI(b)},
		{"eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))},
		{"eth", filterAPI},
	}
	for _, api := range apis {
		if err := s.RegisterName(api.namespace, api.api); err != nil {
			return nil, fmt.Errorf("%T.RegisterName(%q, %T): %v", s, api.namespace, api.api, err)
		}
	}
	return s, nil
}

// web3API offers the `web3` RPCs.
type web3API struct {
	clientVersion string
}

func newWeb3API() *web3API {
	return &web3API{
		clientVersion: version.GetVersions().String(),
	}
}

func (w *web3API) ClientVersion() string {
	return w.clientVersion
}

func (*web3API) Sha3(input hexutil.Bytes) hexutil.Bytes {
	return crypto.Keccak256(input)
}

type ethAPIBackend struct {
	vm             *VM
	ethapi.Backend // TODO(arr4n) remove in favour of `var _ ethapi.Backend = (*ethAPIBackend)(nil)`
	*txgossip.Set
}

func (b *ethAPIBackend) ChainConfig() *params.ChainConfig {
	return b.vm.exec.ChainConfig()
}

func (b *ethAPIBackend) RPCTxFeeCap() float64 {
	return 0 // TODO(arr4n)
}

func (b *ethAPIBackend) UnprotectedAllowed() bool {
	return false
}

func (b *ethAPIBackend) CurrentBlock() *types.Header {
	return types.CopyHeader(b.vm.exec.LastExecuted().Header())
}

func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int {
	return big.NewInt(0) // TODO(arr4n)
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	num, err := b.resolveBlockNumber(n)
	if errors.Is(err, errFutureBlockNotResolved) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return rawdb.ReadBlock(
		b.vm.db,
		rawdb.ReadCanonicalHash(b.vm.db, num),
		num,
	), nil
}

var errFutureBlockNotResolved = errors.New("not accepted yet")

func (b *ethAPIBackend) resolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := b.vm.last.accepted.Load().Height()

	switch bn {
	case rpc.PendingBlockNumber: // i.e. pending execution
		return head, nil
	case rpc.LatestBlockNumber:
		return b.vm.exec.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return b.vm.last.settled.Load().Height(), nil
	}

	if bn < 0 {
		// Any future definitions should be added above.
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > head {
		return 0, fmt.Errorf("%w: block %d", errFutureBlockNotResolved, n)
	}
	return n, nil
}

func (b *ethAPIBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.vm.exec.SubscribeChainEvent(ch)
}

func (b *ethAPIBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.vm.exec.SubscribeChainHeadEvent(ch)
}

func (b *ethAPIBackend) SubscribeChainSideEvent(chan<- core.ChainSideEvent) event.Subscription {
	// SAE never reorgs, so there are no side events.
	return newNoopSubscription()
}

func (b *ethAPIBackend) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return b.Set.Pool.SubscribeTransactions(ch, true)
}

func (b *ethAPIBackend) SubscribeRemovedLogsEvent(chan<- core.RemovedLogsEvent) event.Subscription {
	// SAE never reorgs, so no logs are ever removed.
	return newNoopSubscription()
}

func (b *ethAPIBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return b.vm.exec.SubscribeLogsEvent(ch)
}

func (b *ethAPIBackend) SubscribePendingLogsEvent(chan<- []*types.Log) event.Subscription {
	// In SAE, "pending" refers to the execution status. There are no logs known
	// for transactions pending execution.
	return newNoopSubscription()
}

type noopSubscription struct {
	once sync.Once
	err  chan error
}

func newNoopSubscription() *noopSubscription {
	return &noopSubscription{
		err: make(chan error),
	}
}

func (s *noopSubscription) Err() <-chan error {
	return s.err
}

func (s *noopSubscription) Unsubscribe() {
	s.once.Do(func() {
		close(s.err)
	})
}
