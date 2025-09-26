// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cmputils

import (
	"math/big"

	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
)

// BigInts returns a [cmp.Comparer] for [big.Int] pointers. A nil pointer is not
// equal to zero.
func BigInts() cmp.Option {
	return ComparerWithNilCheck(func(a, b *big.Int) bool {
		return a.Cmp(b) == 0
	})
}

// BlocksByHash returns a [cmp.Comparer] for [types.Block] pointers, equating
// them by hash alone.
func BlocksByHash() cmp.Option {
	return ComparerWithNilCheck(func(b, c *types.Block) bool {
		return b.Hash() == c.Hash()
	})
}

// ReceiptsByTxHash returns a [cmp.Comparer] for [types.Receipt] pointers,
// equating them by transaction hash alone.
func ReceiptsByTxHash() cmp.Option {
	return ComparerWithNilCheck(func(r, s *types.Receipt) bool {
		return r.TxHash == s.TxHash
	})
}
