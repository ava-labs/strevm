package sae

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/arr4n/sink"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

func cmpVMs(ctx context.Context, tb testing.TB) cmp.Options {
	var zeroVM VM

	return cmp.Options{
		blocks.CmpOpt(),
		saexec.CmpOpt(ctx),
		cmp.AllowUnexported(
			VM{},
			last{},
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
		saetest.CmpStateDBs(),
	}
}
