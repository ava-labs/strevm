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
	for _, numAccounts := range []uint{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("accounts=%d", numAccounts), func(b *testing.B) {
			signer := types.LatestSigner(saetest.ChainConfig())
			wallet := saetest.NewUNSAFEWallet(b, numAccounts, signer)
			sut := newSUT(b, saetest.MaxAllocFor(wallet.Addresses()...))

			tx := wallet.SetNonceAndSign(b, 0, &types.LegacyTx{
				To:       &common.Address{},
				Gas:      params.TxGas,
				GasPrice: big.NewInt(1),
			})

			snapTree, err := snapshot.New(
				snapshot.Config{CacheSize: saexec.SnapshotCacheSizeMB},
				sut.DB, sut.StateCache.TrieDB(), sut.Genesis.PostExecutionStateRoot(),
			)
			require.NoError(b, err, "snapshot.New()")
			b.Cleanup(snapTree.Release)

			hdr := &types.Header{
				ParentHash: sut.Genesis.Hash(),
				Number:     big.NewInt(1),
			}

			for _, tt := range []struct {
				name  string
				snaps *snapshot.Tree
			}{
				{name: "without_snapshot", snaps: nil},
				{name: "with_snapshot", snaps: snapTree},
			} {
				b.Run(tt.name, func(b *testing.B) {
					b.StopTimer()
					for range b.N {
						s, err := NewState(sut.Hooks, sut.Config, sut.StateCache, sut.Genesis, tt.snaps)
						require.NoError(b, err, "NewState()")
						require.NoError(b, s.StartBlock(hdr), "StartBlock()")

						b.StartTimer()
						err = s.ApplyTx(tx)
						b.StopTimer()
						require.NoError(b, err, "ApplyTx()")
					}
				})
			}
		})
	}
}
