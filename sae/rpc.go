// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/ethdb"
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

	// Even if this function errors, we should close the quitter to prevent a
	// goroutine from leaking in the filter API.
	quitter := make(chan struct{})
	vm.toClose = append(vm.toClose, func() error {
		close(quitter)
		return nil
	})

	filterSystem := filters.NewFilterSystem(b, filters.Config{})
	for _, ethAPI := range []any{
		ethapi.NewBlockChainAPI(b),
		ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker)),
		filters.NewFilterAPI(filterSystem, false /*isLightClient*/, filters.WithQuitter(quitter)),
	} {
		if err := s.RegisterName("eth", ethAPI); err != nil {
			return nil, fmt.Errorf("%T.RegisterName(%q, %T): %v", s, "eth", ethAPI, err)
		}
	}
	return s, nil
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

func (b *ethAPIBackend) HeaderByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Header, error) {
	return readByNumber(b, n, rawdb.ReadHeader)
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(b, n, rawdb.ReadBlock)
}

type canonicalReader[T any] func(ethdb.Reader, common.Hash, uint64) *T

func readByNumber[T any](b *ethAPIBackend, n rpc.BlockNumber, read canonicalReader[T]) (*T, error) {
	num, err := b.resolveBlockNumber(n)
	if errors.Is(err, errFutureBlockNotResolved) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return read(
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
