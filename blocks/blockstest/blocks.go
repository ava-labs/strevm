// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/saetest"
)

// NewEthBlock constructs a raw Ethereum block with the given arguments.
func NewEthBlock(parent *types.Block, time uint64, txs types.Transactions) *types.Block {
	hdr := &types.Header{
		Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
		Time:       time,
		ParentHash: parent.Hash(),
	}
	return types.NewBlock(hdr, txs, nil, nil, saetest.TrieHasher())
}

// NewBlock constructs an SAE block, wrapping the raw Ethereum block.
func NewBlock(tb testing.TB, eth *types.Block, parent, lastSettled *blocks.Block) *blocks.Block {
	tb.Helper()
	b, err := blocks.New(eth, parent, lastSettled, saetest.NewTBLogger(tb, logging.Warn))
	require.NoError(tb, err, "New()")
	return b
}

// NewGenesis constructs a new [core.Genesis], writes it to the database, and
// returns wraps [core.Genesis.ToBlock] with [NewBlock]. It assumes a nil
// [triedb.Config]. The block is marked as both executed and synchronous.
func NewGenesis(tb testing.TB, db ethdb.Database, config *params.ChainConfig, alloc types.GenesisAlloc) *blocks.Block {
	tb.Helper()
	var _ *triedb.Config = nil // protect the import to allow linked function comment

	gen := &core.Genesis{
		Config: config,
		Alloc:  alloc,
	}

	tdb := state.NewDatabase(db).TrieDB()
	_, hash, err := core.SetupGenesisBlock(db, tdb, gen)
	require.NoError(tb, err, "core.SetupGenesisBlock()")
	require.NoErrorf(tb, tdb.Commit(hash, true), "%T.Commit(core.SetupGenesisBlock(...))", tdb)

	b := NewBlock(tb, gen.ToBlock(), nil, nil)
	require.NoErrorf(tb, b.MarkExecuted(db, gastime.New(0, 1, 0), time.Time{}, new(big.Int), nil, b.SettledStateRoot()), "%T.MarkExecuted()", b)
	require.NoErrorf(tb, b.MarkSynchronous(), "%T.MarkSynchronous()", b)
	return b
}
