// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cmputils

import (
	"math/big"

	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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

// TransactionsByHash returns a [cmp.Comparer] for [types.Transaction] pointers,
// equating them by hash alone.
func TransactionsByHash() cmp.Option {
	return ComparerWithNilCheck(func(t, u *types.Transaction) bool {
		return t.Hash() == u.Hash()
	})
}

// ReceiptsByTxHash returns a [cmp.Comparer] for [types.Receipt] pointers,
// equating them by transaction hash alone.
func ReceiptsByTxHash() cmp.Option {
	return ComparerWithNilCheck(func(r, s *types.Receipt) bool {
		return r.TxHash == s.TxHash
	})
}

// EthBlocks returns a set of [cmp.Options] for comparing [types.Block] values.
// You must also use [Headers] to compare the block headers.
func EthBlocks() cmp.Option {
	return cmp.Options{
		cmp.AllowUnexported(types.Block{}, types.Header{}),
		cmpopts.IgnoreFields(types.Block{}, "ReceivedAt", "ReceivedFrom", "hash", "size", "extra"),
		IfIn[types.Block](TransactionsByHash()),
	}
}

// Headers returns a set of [cmp.Options] for comparing [type.Headers] values.
func Headers() cmp.Option {
	return cmp.Options{
		cmpopts.IgnoreFields(types.Header{}, "extra"),
		// Without the [IfIn] filter, any other use of [BigInts] will result in
		// ambiguous comparers as [cmp] can't deduplicate them.
		IfIn[types.Header](BigInts()),
	}
}
