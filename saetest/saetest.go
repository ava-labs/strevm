// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saetest provides testing helpers for [Streaming Asynchronous
// Execution] (SAE).
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saetest

import (
	"slices"
	"sync"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/trie"
	"github.com/google/go-cmp/cmp"

	"github.com/ava-labs/strevm/hook"
)

// TrieHasher returns an arbitrary trie hasher.
func TrieHasher() types.TrieHasher {
	return trie.NewStackTrie(nil)
}

// MerkleRootsEqual returns whether the two arguments have the same Merkle root.
func MerkleRootsEqual[T types.DerivableList](a, b T) bool {
	return types.DeriveSha(a, TrieHasher()) == types.DeriveSha(b, TrieHasher())
}

// CmpByMerkleRoots returns a [cmp.Comparer] using [MerkleRootsEqual].
func CmpByMerkleRoots[T types.DerivableList]() cmp.Option {
	return cmp.Comparer(MerkleRootsEqual[T])
}

// An EventCollector collects all events received from an [event.Subscription].
// All methods are safe for concurrent use.
type EventCollector[T any] struct {
	ch   chan T
	done chan struct{}
	sub  event.Subscription

	all  []T
	cond *sync.Cond
}

// NewEventCollector returns a new [EventCollector], subscribing via the
// provided function. [EventCollector.Unsubscribe] must be called to release
// resources.
func NewEventCollector[T any](subscribe func(chan<- T) event.Subscription) *EventCollector[T] {
	c := &EventCollector[T]{
		ch:   make(chan T),
		done: make(chan struct{}),
		cond: sync.NewCond(&sync.Mutex{}),
	}
	c.sub = subscribe(c.ch)
	go c.collect()
	return c
}

func (c *EventCollector[T]) collect() {
	defer close(c.done)
	for x := range c.ch {
		c.cond.L.Lock()
		c.all = append(c.all, x)
		c.cond.L.Unlock()
		c.cond.Broadcast()
	}
}

// All returns all events received thus far.
func (c *EventCollector[T]) All() []T {
	c.cond.L.Lock()
	defer c.cond.L.Unlock()
	return slices.Clone(c.all)
}

// Unsubscribe unsubscribes from the subscription and returns the error,
// possibly nil, received on [event.Subscription.Err].
func (c *EventCollector[T]) Unsubscribe() error {
	c.sub.Unsubscribe()
	err := <-c.sub.Err()
	close(c.ch)
	<-c.done
	return err
}

// WaitForAtLeast blocks until at least `n` events have been received.
func (c *EventCollector[T]) WaitForAtLeast(n int) {
	c.cond.L.Lock()
	for len(c.all) < n {
		c.cond.Wait()
	}
	c.cond.L.Unlock()
}

// HookStub implements [hook.Points].
type HookStub struct {
	Target gas.Gas
}

var _ hook.Points = (*HookStub)(nil)

// GasTarget ignores its argument and always returns [HookStub.Target].
func (s *HookStub) GasTarget(*types.Block) gas.Gas { return s.Target }
