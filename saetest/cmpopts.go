//go:build !prod && !nocmpopts

// Package saetest provides utilities for Streaming Asynchronous Execution (SAE)
// tests.
package saetest

import (
	"math/big"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// ComparerWithNilCheck returns a function that returns:
//
// - true if both a and b are nil
//
// - false if exactly one of a or b are nil
//
// - fn(x,y) if neither x nor y are nil
func ComparerWithNilCheck[T any](fn func(*T, *T) bool) func(a, b *T) bool {
	return func(a, b *T) bool {
		switch an, bn := a == nil, b == nil; {
		case an && bn:
			return true
		case an || bn:
			return false
		}
		return fn(a, b)
	}
}

// ComparerOptWithNilCheck is a convenience function for wrapping
// [ComparerWithNilCheck] with [cmp.Comparer].
func ComparerOptWithNilCheck[T any](fn func(*T, *T) bool) cmp.Option {
	return cmp.Comparer(ComparerWithNilCheck(fn))
}

// CmpReceiptsByTxHash compares two [types.Receipt] pointers by transaction
// hash.
func CmpReceiptsByTxHash() cmp.Option {
	return ComparerOptWithNilCheck(func(a, b *types.Receipt) bool {
		return a.TxHash == b.TxHash
	})
}

func trieHasher() types.TrieHasher {
	return trie.NewStackTrie(nil)
}

// CmpReceiptsByMerkleRoot compares two [types.Receipts] slices by their Merkle
// roots as computed with [types.DeriveSha], using [trie.NewStackTrie] as the
// [types.TrieHasher].
func CmpReceiptsByMerkleRoot() cmp.Option {
	return cmp.Comparer(func(a, b types.Receipts) bool {
		return types.DeriveSha(a, trieHasher()) == types.DeriveSha(b, trieHasher())
	})
}

// CmpBigInts compares [big.Int] values with [big.Int.Cmp] i.f.f. both are
// non-nil. See [ComparerWithNilCheck].
func CmpBigInts() cmp.Option {
	return ComparerOptWithNilCheck(func(a, b *big.Int) bool {
		return a.Cmp(b) == 0
	})
}

// CmpStateDBs compares [state.StateDB] instances by transforming them to
// [state.Dump] instances.
func CmpStateDBs() cmp.Option {
	return cmp.Transformer("state_dump", func(db *state.StateDB) state.Dump {
		db = db.Copy()
		return db.RawDump(&state.DumpConfig{})
	})
}

// CmpTimes combines [gastime.CmpOpt] and [proxytime.CmpOpt] with the latter
// receiving a [gas.Gas] type parameter.
func CmpTimes() cmp.Option {
	return cmp.Options{
		gastime.CmpOpt(),
		proxytime.CmpOpt[gas.Gas](),
	}
}

// CmpChainConfig ignores the libevm payload of [params.ChainConfig] instances.
// It SHOULD be used in conjunction with [CmpBigInts].
func CmpChainConfig() cmp.Option {
	return cmp.Options{
		cmpopts.IgnoreUnexported(params.ChainConfig{}), // libevm payload
	}
}

// CmpIgnoreLowLevelDBs ignores all implementations of the [ethdb.Database] and
// [state.Database] interfaces.
func CmpIgnoreLowLevelDBs() cmp.Option {
	return cmp.Options{
		cmpopts.IgnoreInterfaces(struct{ ethdb.Database }{}),
		cmpopts.IgnoreInterfaces(struct{ state.Database }{}),
	}
}

// CmpIgnoreLoggers ignores all implementations of the [logging.Logger]
// interface.
func CmpIgnoreLoggers() cmp.Option {
	return cmpopts.IgnoreInterfaces(struct{ logging.Logger }{})
}
