// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
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

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/holiman/uint256"
)

func trieHasher() types.TrieHasher {
	return trie.NewStackTrie(nil)
}

func unix(t time.Time) uint64 {
	return uint64(t.Unix()) //nolint:gosec // Guaranteed to be positive
}

type nilAllowed bool

const (
	allowNil nilAllowed = true
	errOnNil nilAllowed = false
)

// uint256FromBig is a wrapper around [uint256.FromBig] with extra checks.
func uint256FromBig(b *big.Int, nilHandling nilAllowed) (*uint256.Int, error) {
	if b == nil && nilHandling == errOnNil {
		return nil, errors.New("nil big.Int")
	}
	u, overflow := uint256.FromBig(b)
	if overflow {
		return nil, fmt.Errorf("big.Int %v overflows 256 bits", b)
	}
	return u, nil
}

type sMap[K comparable, V any] struct {
	m sync.Map
}

func (m *sMap[K, V]) zeroValue() V {
	var zero V
	return zero
}

func (m *sMap[K, V]) Load(k K) (V, bool) {
	v, ok := m.m.Load(k)
	if !ok {
		return m.zeroValue(), false
	}
	return v.(V), true //nolint:forcetypeassert // Known invariant
}

func (m *sMap[K, V]) Store(k K, v V) {
	m.m.Store(k, v)
}

func (m *sMap[K, V]) Delete(k K) {
	m.m.Delete(k)
}
