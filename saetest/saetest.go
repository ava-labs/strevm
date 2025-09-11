// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saetest provides testing helpers for [Streaming Asynchronous
// Execution] (SAE).
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saetest

import (
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/google/go-cmp/cmp"
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
