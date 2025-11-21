// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"slices"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/txpool"
)

// A LazyTransaction couples a [txpool.LazyTransaction] with its sender.
type LazyTransaction struct {
	*txpool.LazyTransaction
	Sender common.Address
}

// TransactionsByPriority calls [txpool.TxPool.Pending] with the given filter,
// collapses the results into a slice, and sorts said slice by decreasing gas
// tip then chronologically. Transactions from the same sender are merely sorted
// by increasing nonce.
func (s *Set) TransactionsByPriority(filter txpool.PendingFilter) []*LazyTransaction {
	pending := s.Pool.Pending(filter)
	var n int
	for _, txs := range pending {
		n += len(txs)
	}

	all := make([]*LazyTransaction, n)
	var i int
	for from, txs := range pending {
		for _, tx := range txs {
			all[i] = &LazyTransaction{
				LazyTransaction: tx,
				Sender:          from,
			}
			i++
		}
	}

	slices.SortStableFunc(all, func(a, b *LazyTransaction) int {
		if a.Sender == b.Sender {
			// [txpool.TxPool.Pending] already returns each slice in nonce order
			// and we're performing a stable sort.
			return 0
		}
		if tip := a.GasTipCap.Cmp(b.GasTipCap); tip != 0 {
			return -tip // Higher tips first
		}
		return a.Time.Compare(b.Time)
	})
	return all
}
