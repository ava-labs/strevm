package rpc

import (
	"sync"

	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
)

func (b *apiBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.vm.SubscribeChainEvent(ch)
}

func (b *apiBackend) SubscribeChainSideEvent(chan<- core.ChainSideEvent) event.Subscription {
	// SAE never reorgs, so there are no side events.
	return newNoopSubscription()
}

func (b *apiBackend) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return b.Set.Pool.SubscribeTransactions(ch, true)
}

func (b *apiBackend) SubscribeRemovedLogsEvent(chan<- core.RemovedLogsEvent) event.Subscription {
	// SAE never reorgs, so no logs are ever removed.
	return newNoopSubscription()
}

func (b *apiBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return b.vm.SubscribeLogsEvent(ch)
}

func (b *apiBackend) SubscribePendingLogsEvent(chan<- []*types.Log) event.Subscription {
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
