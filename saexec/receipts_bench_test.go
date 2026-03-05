// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ava-labs/libevm/common"
)

func BenchmarkReceiptChannels(b *testing.B) {
	for _, txCount := range []int{1, 10, 100, 500} {
		txHashes := make([]common.Hash, txCount)
		for i := range txHashes {
			txHashes[i] = common.Hash{byte(i), byte(i >> 8)}
		}

		b.Run(fmt.Sprintf("txs=%03d/alloc", txCount), func(b *testing.B) {
			b.ReportAllocs()
			m := newSyncMap[common.Hash, chan *Receipt]()
			for range b.N {
				m.StoreFromFunc(func(common.Hash) chan *Receipt {
					return make(chan *Receipt, 1)
				}, txHashes...)
				for _, h := range txHashes {
					m.Load(h)
				}
				m.Delete(txHashes...)
			}
		})

		b.Run(fmt.Sprintf("txs=%03d/pool", txCount), func(b *testing.B) {
			b.ReportAllocs()
			pool := sync.Pool{
				New: func() any { return make(chan *Receipt, 1) },
			}
			m := newSyncMap[common.Hash, chan *Receipt]()
			for range b.N {
				m.StoreFromFunc(func(common.Hash) chan *Receipt {
					return pool.Get().(chan *Receipt) //nolint:forcetypeassert // New always returns chan *Receipt
				}, txHashes...)
				for _, h := range txHashes {
					if ch, ok := m.Load(h); ok {
						select {
						case <-ch:
						default:
						}
						pool.Put(ch)
					}
				}
				m.Delete(txHashes...)
			}
		})
	}
}
