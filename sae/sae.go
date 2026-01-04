// Copyright (C) ((20\d\d\-2026)|(2026)), Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package sae implements the [Streaming Asynchronous Execution] (SAE) virtual
// machine to be compatible with Avalanche consensus.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package sae

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/holiman/uint256"
)

func unix(t time.Time) uint64 {
	return uint64(t.Unix()) //nolint:gosec // Guaranteed to be positive
}

// uint256FromBig is a wrapper around [uint256.FromBig] with extra checks, for
// nil input and for overflow.
func uint256FromBig(b *big.Int) (*uint256.Int, error) {
	if b == nil {
		return nil, errors.New("nil big.Int")
	}
	u, overflow := uint256.FromBig(b)
	if overflow {
		return nil, fmt.Errorf("big.Int %v overflows 256 bits", b)
	}
	return u, nil
}

type sMap[K comparable, V any] struct {
	m  map[K]V
	mu sync.RWMutex
}

func newSMap[K comparable, V any]() *sMap[K, V] {
	return &sMap[K, V]{
		m: make(map[K]V),
	}
}

func (m *sMap[K, V]) Load(k K) (V, bool) {
	m.mu.RLock()
	v, ok := m.m[k]
	m.mu.RUnlock()
	return v, ok
}

func (m *sMap[K, V]) Store(k K, v V) {
	m.mu.Lock()
	m.m[k] = v
	m.mu.Unlock()
}

func (m *sMap[K, V]) Delete(k K) {
	m.mu.Lock()
	delete(m.m, k)
	m.mu.Unlock()
}
