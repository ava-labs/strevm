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

// CommitTrieDBEvery is the number of blocks between commits of the state
// trie to disk.
const CommitTrieDBEvery = 4096

// CommitIntervalOrDefault returns the configured trie commit interval.
// If no test override was set, it returns [CommitTrieDBEvery].
func (c Config) CommitIntervalOrDefault() uint64 {
	if c.commitInterval == 0 {
		return CommitTrieDBEvery
	}
	return c.commitInterval
}

// SetCommitIntervalForTesting overrides the trie commit interval used by this
// config. This is intended to ONLY be used for tests as production callers
// should rely on the default.
func (c *Config) SetCommitIntervalForTesting(interval uint64) {
	c.commitInterval = interval
}

// ShouldCommitTrieDB returns whether or not to commit the state trie to disk.
func (c Config) ShouldCommitTrieDB(blockNum uint64) bool {
	return blockNum%c.CommitIntervalOrDefault() == 0
}

// LastCommittedTrieDBHeight returns the largest value <= the argument at which
// [Config.ShouldCommitTrieDB] would have returned true.
func (c Config) LastCommittedTrieDBHeight(atOrBefore uint64) uint64 {
	interval := c.CommitIntervalOrDefault()
	return atOrBefore / interval * interval
}

// A StateDBOpener opens a [state.StateDB] at the given root.
type StateDBOpener interface {
	StateDB(root common.Hash) (*state.StateDB, error)
}
