package sae

import (
	"context"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/arr4n/sink"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saexec"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

func cmpReceiptsByHash() cmp.Options {
	return cmp.Options{
		cmp.Transformer("receiptHash", func(r *types.Receipt) common.Hash {
			if r == nil {
				return common.Hash{}
			}
			return r.TxHash
		}),
	}
}

func cmpBigInts() cmp.Options {
	return cmp.Options{
		cmp.Comparer(func(a, b *big.Int) bool {
			if a == nil || b == nil {
				return a == nil && b == nil
			}
			return a.Cmp(b) == 0
		}),
	}
}

func cmpTimes() cmp.Options {
	return cmp.Options{
		cmp.AllowUnexported(
			gastime.TimeMarshaler{},
			proxytime.Time[gas.Gas]{},
		),
		cmpopts.IgnoreFields(gastime.TimeMarshaler{}, "canotoData"),
		cmpopts.IgnoreFields(proxytime.Time[gas.Gas]{}, "canotoData"),
	}
}

func cmpBlocks() cmp.Options {
	return cmp.Options{
		cmpBigInts(),
		cmpTimes(),
		cmp.Comparer(func(a, b *types.Block) bool {
			return a.Hash() == b.Hash()
		}),
		cmp.Comparer(func(a, b types.Receipts) bool {
			return types.DeriveSha(a, trieHasher()) == types.DeriveSha(b, trieHasher())
		}),
		cmpopts.EquateComparable(
			// We're not running tests concurrently with anything that will
			// modify [Block.accepted] nor [Block.executed] so this is safe.
			// Using a [cmp.Transformer] would make the linter complain about
			// copying.
			atomic.Bool{},
		),
	}
}

func cmpVMs(ctx context.Context, tb testing.TB) cmp.Options {
	var zeroVM VM

	return cmp.Options{
		cmpBlocks(),
		cmp.AllowUnexported(
			VM{},
			last{},
			saexec.Executor{},
		),
		cmpopts.IgnoreUnexported(params.ChainConfig{}),
		cmpopts.IgnoreFields(VM{}, "preference"),
		cmpopts.IgnoreTypes(
			zeroVM.snowCtx,
			zeroVM.mempool,
			&snapshot.Tree{},
		),
		cmpopts.IgnoreInterfaces(struct{ snowcommon.AppHandler }{}),
		cmpopts.IgnoreInterfaces(struct{ ethdb.Database }{}),
		cmpopts.IgnoreInterfaces(struct{ logging.Logger }{}),
		cmpopts.IgnoreInterfaces(struct{ state.Database }{}),
		cmp.Transformer("block_map_mu", func(mu sink.Mutex[blockMap]) blockMap {
			bm, err := sink.FromMutex(ctx, mu, func(bm blockMap) (blockMap, error) {
				return bm, nil
			})
			require.NoError(tb, err)
			return bm
		}),
		cmp.Transformer("atomic_block", func(p atomic.Pointer[blocks.Block]) *blocks.Block {
			return p.Load()
		}),
		cmp.Transformer("state_dump", func(db *state.StateDB) state.Dump {
			return db.RawDump(&state.DumpConfig{})
		}),
	}
}
