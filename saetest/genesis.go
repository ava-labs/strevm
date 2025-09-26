// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saetest

import (
	"crypto/ecdsa"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

// Genesis constructs a new [core.Genesis], writes it to the database, and
// returns [core.Genesis.ToBlock]. It assumes a nil [triedb.Config].
func Genesis(tb testing.TB, db ethdb.Database, config *params.ChainConfig, alloc types.GenesisAlloc) *types.Block {
	tb.Helper()
	var _ *triedb.Config = nil // protect the import to allow linked function comment

	gen := &core.Genesis{
		Config: config,
		Alloc:  alloc,
	}

	tdb := state.NewDatabase(db).TrieDB()
	_, hash, err := core.SetupGenesisBlock(db, tdb, gen)
	require.NoError(tb, err, "core.SetupGenesisBlock()")
	require.NoErrorf(tb, tdb.Commit(hash, true), "%T.Commit(core.SetubGenesisBlock(...))", tdb)

	return gen.ToBlock()
}

// MaxUint256 returns 2^256-1.
func MaxUint256() *uint256.Int {
	return new(uint256.Int).SetAllOne()
}

// MaxAllocFor returns a genesis allocation with [MaxUint256] as the balance for
// all addresses provided.
func MaxAllocFor(addrs ...common.Address) types.GenesisAlloc {
	alloc := make(types.GenesisAlloc)
	for _, a := range addrs {
		alloc[a] = types.Account{
			Balance: MaxUint256().ToBig(),
		}
	}
	return alloc
}

// KeyWithMaxAlloc generates a new [crypto.S256] key and returns it along with
// a genesis alloc providing the key's address with a balance of [MaxUint256].
func KeyWithMaxAlloc(tb testing.TB) (*ecdsa.PrivateKey, types.GenesisAlloc) {
	tb.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(tb, err, "crypto.GenerateKey()")
	return key, MaxAllocFor(crypto.PubkeyToAddress(key.PublicKey))
}
