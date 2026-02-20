// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"fmt"
	"math"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

func BenchmarkApplyTxWithSnapshot(b *testing.B) {
	config := saetest.ChainConfig()

	key, err := crypto.GenerateKey()
	require.NoError(b, err)
	sender := crypto.PubkeyToAddress(key.PublicKey)

	tx := types.MustSignNewTx(key, types.LatestSigner(config), &types.LegacyTx{
		Nonce:    0,
		To:       &common.Address{},
		Gas:      params.TxGas,
		GasPrice: big.NewInt(1),
	})

	for _, numAccounts := range []int{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("accounts=%d", numAccounts), func(b *testing.B) {
			alloc := make(types.GenesisAlloc, numAccounts+1)
			alloc[sender] = types.Account{
				Balance: new(big.Int).SetUint64(math.MaxUint64),
			}
			// Stay away from low addresses so these accounts addresses
			// are clearly separate.
			for i := 0; i < numAccounts; i++ {
				addr := common.BigToAddress(big.NewInt(int64(i + 1000)))
				alloc[addr] = types.Account{Balance: big.NewInt(1)}
			}

			db := rawdb.NewMemoryDatabase()
			stateCache := state.NewDatabase(db)

			genesis := blockstest.NewGenesis(
				b, db, saetest.NewExecutionResultsDB(), config, alloc,
				blockstest.WithGasTarget(initialGasTarget),
				blockstest.WithGasExcess(initialExcess),
			)
			root := genesis.PostExecutionStateRoot()

			snaps, err := snapshot.New(
				snapshot.Config{CacheSize: saexec.SnapshotCacheSizeMB},
				db, stateCache.TrieDB(), root,
			)
			require.NoError(b, err)
			b.Cleanup(snaps.Release)

			hooks := &hookstest.Stub{Target: initialGasTarget}
			header := &types.Header{
				ParentHash: genesis.Hash(),
				Number:     big.NewInt(1),
			}

			for _, tt := range []struct {
				name  string
				snaps *snapshot.Tree
			}{
				{name: "without_snapshot", snaps: nil},
				{name: "with_snapshot", snaps: snaps},
			} {
				b.Run(tt.name, func(b *testing.B) {
					b.ResetTimer()
					for range b.N {
						s, err := NewState(hooks, config, stateCache, genesis, tt.snaps)
						if err != nil {
							b.Fatal(err)
						}
						if err := s.StartBlock(header); err != nil {
							b.Fatal(err)
						}
						if err := s.ApplyTx(tx); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
