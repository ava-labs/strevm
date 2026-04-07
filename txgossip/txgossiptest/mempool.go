// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package txgossiptest provides test helpers for mempool operations.
package txgossiptest

import (
	"context"
	"testing"

	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
)

// MustAddToMempool performs `addTx` and waits until `pool` marks all transactions provided as pending.
func MustAddToMempool(tb testing.TB, ctx context.Context, pool *txpool.TxPool, addTx func(testing.TB, ...*types.Transaction), txs ...*types.Transaction) {
	tb.Helper()

	if len(txs) == 0 {
		return
	}

	txCh := make(chan core.NewTxsEvent, len(txs))
	sub := pool.SubscribeTransactions(txCh, true /*reorgs but ignored by legacypool*/)
	defer sub.Unsubscribe()

	addTx(tb, txs...)
	set := toSet(txs, (*types.Transaction).Hash)
	for {
		select {
		case <-ctx.Done():
			tb.Fatalf("%v waiting for %T.SubscribeTransactions()", context.Cause(ctx), pool)
		case err := <-sub.Err():
			tb.Fatalf("%T.SubscribeTransactions.Err() returned %v", pool, err)
		case txEvent := <-txCh:
			for _, tx := range txEvent.Txs {
				delete(set, tx.Hash())
			}

			if len(set) == 0 {
				return
			}
		}
	}
}

func toSet[T any, I comparable](list []T, indexer func(T) I) map[I]struct{} {
	m := make(map[I]struct{}, len(list))
	for _, t := range list {
		m[indexer(t)] = struct{}{}
	}
	return m
}
