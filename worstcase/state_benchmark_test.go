// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"fmt"
	"math"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/ethtest"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

type benchSUT struct {
	config     *params.ChainConfig
	stateCache state.Database
	genesis    *blocks.Block
	snaps      *snapshot.Tree
	hooks      *hookstest.Stub
	header     *types.Header
}

func newBenchSUT(b *testing.B, sender common.Address, numAccounts int) benchSUT {
	b.Helper()

	config := saetest.ChainConfig()
	db, stateCache, _ := ethtest.NewEmptyStateDB(b)

	alloc := make(types.GenesisAlloc, numAccounts+1)
	alloc[sender] = types.Account{
		Balance: new(big.Int).SetUint64(math.MaxUint64),
	}
	// Stay away from low addresses so these account addresses
	// are clearly separate.
	for i := range numAccounts {
		addr := common.BigToAddress(big.NewInt(int64(i + 1000)))
		alloc[addr] = types.Account{Balance: big.NewInt(1)}
	}

	genesis := blockstest.NewGenesis(
		b, db, saetest.NewExecutionResultsDB(), config, alloc,
		blockstest.WithGasTarget(initialGasTarget),
		blockstest.WithGasExcess(initialExcess),
	)

	snaps, err := snapshot.New(
		snapshot.Config{CacheSize: saexec.SnapshotCacheSizeMB},
		db, stateCache.TrieDB(), genesis.PostExecutionStateRoot(),
	)
	require.NoError(b, err)
	b.Cleanup(snaps.Release)

	return benchSUT{
		config:     config,
		stateCache: stateCache,
		genesis:    genesis,
		snaps:      snaps,
		hooks:      &hookstest.Stub{Target: initialGasTarget},
		header: &types.Header{
			ParentHash: genesis.Hash(),
			Number:     big.NewInt(1),
		},
	}
}

func BenchmarkApplyTxWithSnapshot(b *testing.B) {
	key, err := crypto.GenerateKey()
	require.NoError(b, err)
	sender := crypto.PubkeyToAddress(key.PublicKey)

	tx := types.MustSignNewTx(key, types.LatestSigner(saetest.ChainConfig()), &types.LegacyTx{
		Nonce:    0,
		To:       &common.Address{},
		Gas:      params.TxGas,
		GasPrice: big.NewInt(1),
	})

	for _, numAccounts := range []int{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("accounts=%d", numAccounts), func(b *testing.B) {
			sut := newBenchSUT(b, sender, numAccounts)

			for _, tt := range []struct {
				name  string
				snaps *snapshot.Tree
			}{
				{name: "without_snapshot", snaps: nil},
				{name: "with_snapshot", snaps: sut.snaps},
			} {
				b.Run(tt.name, func(b *testing.B) {
					for range b.N {
						s, err := NewState(sut.hooks, sut.config, sut.stateCache, sut.genesis, tt.snaps)
						require.NoError(b, err)
						require.NoError(b, s.StartBlock(sut.header))
						require.NoError(b, s.ApplyTx(tx))
					}
				})
			}
		})
	}
}
