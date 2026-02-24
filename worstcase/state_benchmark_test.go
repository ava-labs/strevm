// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package worstcase

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

func BenchmarkApplyTxWithSnapshot(b *testing.B) {
	for _, numAccounts := range []uint{100, 1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("accounts=%d", numAccounts), func(b *testing.B) {
			config := saetest.ChainConfig()
			wallet := saetest.NewUNSAFEWallet(b, numAccounts, types.LatestSigner(config))
			alloc := saetest.MaxAllocFor(wallet.Addresses()...)

			sut := newSUT(b, alloc)

			snaps, err := snapshot.New(
				snapshot.Config{
					CacheSize:  saexec.SnapshotCacheSizeMB,
					AsyncBuild: false,
				},
				sut.db,
				sut.stateCache.TrieDB(),
				sut.Genesis.PostExecutionStateRoot(),
			)
			require.NoError(b, err, "snapshot.New()")
			b.Cleanup(snaps.Release)

			hdr := &types.Header{
				ParentHash: sut.Genesis.Hash(),
				Number:     new(big.Int).Add(sut.Genesis.Number(), common.Big1),
			}
			tx := wallet.SetNonceAndSign(b, 0, &types.LegacyTx{
				To:       &common.Address{},
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
			})

			for _, tt := range []struct {
				name  string
				snaps *snapshot.Tree
			}{
				{name: "without_snapshot", snaps: nil},
				{name: "with_snapshot", snaps: snaps},
			} {
				b.Run(tt.name, func(b *testing.B) {
					b.StopTimer()
					for range b.N {
						s, err := NewState(sut.Hooks, config, sut.stateCache, sut.Genesis, tt.snaps)
						require.NoError(b, err, "NewState()")
						require.NoErrorf(b, s.StartBlock(hdr), "%T.StartBlock()", s)

						b.StartTimer()
						err = s.ApplyTx(tx)
						b.StopTimer()
						require.NoErrorf(b, err, "%T.ApplyTx()", s)
					}
				})
			}
		})
	}
}
