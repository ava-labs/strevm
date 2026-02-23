// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package cache provides a thread-safe key-value store.
package cache

import (
	"cmp"
	"slices"
	"sync"
)

// A UniformlyKeyed cache holds values keyed by uniformly distributed keys,
// allowing for reduced lock contention.
type UniformlyKeyed[K ~[32]byte, V any] struct {
	buckets [256]bucket[K, V]
}

type bucket[K comparable, V any] struct {
	sync.RWMutex
	data map[K]V
}

// NewUniformlyKeyed constructs a new [UniformlyKeyed] cache.
func NewUniformlyKeyed[K ~[32]byte, V any]() *UniformlyKeyed[K, V] {
	c := new(UniformlyKeyed[K, V])
	for i := range 256 {
		c.buckets[i].data = make(map[K]V)
	}
	return c
}

// Store stores the key and value.
func (c *UniformlyKeyed[K, V]) Store(k K, v V) {
	b := &c.buckets[k[0]]
	b.Lock()
	b.data[k] = v
	b.Unlock()
}

// Load returns a previously stored value and a boolean indicating if it was
// found in the cache.
func (c *UniformlyKeyed[K, V]) Load(k K) (V, bool) {
	b := &c.buckets[k[0]]
	b.RLock()
	v, ok := b.data[k]
	b.RUnlock()
	return v, ok
}

// Delete removes all provided keys from the cache.
func (c *UniformlyKeyed[K, V]) Delete(ks ...K) {
	if len(ks) == 0 {
		return
	}
	slices.SortFunc(ks, func(a, b K) int {
		return cmp.Compare(a[0], b[0])
	})

	var (
		index  byte
		locked *bucket[K, V]
	)
	for _, k := range ks {
		if idx := k[0]; locked == nil || idx != index {
			if locked != nil {
				locked.Unlock()
			}
			index = idx
			locked = &c.buckets[idx]
			locked.Lock()
		}
		delete(locked.data, k)
	}
	locked.Unlock()
}

// Clear removes all keys from the cache.
func (c *UniformlyKeyed[K, V]) Clear() {
	for i := range len(c.buckets) {
		b := &c.buckets[i]
		b.Lock()
		defer b.Unlock()
		b.data = make(map[K]V)
	}
}
