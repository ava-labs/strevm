// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saedb provides functionality related to storage and access of
// [Streaming Asynchronous Execution] data.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saedb

import (
	"encoding/binary"

	// Imported for [rawdb] comment-link resolution.
	_ "github.com/ava-labs/libevm/core/rawdb"
)

const rawDBPrefix = "\x00\x00-ava-sae-"

// RawDBKeyForBlock returns an SAE-specific key for use with the [rawdb]
// package.
func RawDBKeyForBlock(namespace string, num uint64) []byte {
	n := len(rawDBPrefix) + len(namespace) + 1 /*hyphen*/
	key := make([]byte, n, n+8)

	copy(key, []byte(rawDBPrefix))
	copy(key[len(rawDBPrefix):], []byte(namespace))

	key[n-1] = '-'
	return binary.BigEndian.AppendUint64(key, num)
}

const (
	// CommitTrieDBEvery is the number of blocks between commits of the state
	// trie to disk.
	CommitTrieDBEvery     = 1 << commitTrieDBEveryLog2
	commitTrieDBEveryLog2 = 12
	commitTrieDBMask      = CommitTrieDBEvery - 1
)

// ShouldCommitTrieDB returns whether or not to commit the state trie to disk.
func ShouldCommitTrieDB(blockNum uint64) bool {
	return blockNum&commitTrieDBMask == 0
}

// LastCommittedTrieDBHeight returns the largest value <= the argument at which
// [ShouldCommitTrieDB] would have returned true.
func LastCommittedTrieDBHeight(atOrBefore uint64) uint64 {
	return atOrBefore &^ commitTrieDBMask
}
