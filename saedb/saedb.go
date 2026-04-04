// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package saedb provides functionality related to storage and access of
// [Streaming Asynchronous Execution] data.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package saedb

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
)

// defaultCommitInterval is the default number of blocks between commits of the
// state trie to disk.
const defaultCommitInterval = 4096

// CommitInterval returns the trie commit interval.
func (c Config) CommitInterval() uint64 {
	if c.TrieCommitInterval == 0 {
		return defaultCommitInterval
	}
	return c.TrieCommitInterval
}

// ShouldCommitTrieDB returns whether or not to commit the state trie to disk.
func (c Config) ShouldCommitTrieDB(blockNum uint64) bool {
	return blockNum%c.CommitInterval() == 0
}

// LastCommittedTrieDBHeight returns the largest value <= the argument at which
// [Config.ShouldCommitTrieDB] would have returned true.
func (c Config) LastCommittedTrieDBHeight(atOrBefore uint64) uint64 {
	return atOrBefore - atOrBefore%c.CommitInterval()
}

// A StateDBOpener opens a [state.StateDB] at the given root.
type StateDBOpener interface {
	StateDB(root common.Hash) (*state.StateDB, error)
}
