// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package params declares [Streaming Asynchronous Execution] (SAE) parameters.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package params

import (
	"encoding/binary"
	"time"

	// Imported for [rawdb] comment-link resolution.
	_ "github.com/ava-labs/libevm/core/rawdb"
)

// Lambda is the denominator for computing the minimum gas consumed per
// transaction. For a transaction with gas limit `g`, the minimum consumption is
// ceil(g/Lambda).
const Lambda = 2

// Tau is the minimum duration between a block's execution completing and the
// resulting state changes being settled in a later block. Note that this period
// has no effect on the availability nor finality of results, both of which are
// immediate at the time of executing an individual transaction.
const (
	Tau        = TauSeconds * time.Second
	TauSeconds = 5
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

// CommitTrieDB returns whether or not to commit the state trie to disk.
func CommitTrieDB(blockNum uint64) bool {
	return blockNum > 0 && blockNum&commitTrieDBMask == 0
}

// LastCommittedTrieDBHeight returns the largest value <= the argument at which
// [CommitTrieDB] would have returned true.
func LastCommittedTrieDBHeight(atOrBefore uint64) uint64 {
	return atOrBefore &^ commitTrieDBMask
}
