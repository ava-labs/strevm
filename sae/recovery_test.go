package sae

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/memdb"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	saeparams "github.com/ava-labs/strevm/params"
)

func TestRecoverFromDatabase(t *testing.T) {
	sutOpt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))

	var srcDB database.Database
	ctx, src := newSUT(t, 3, sutOpt, options.Func[sutConfig](func(c *sutConfig) {
		srcDB = c.db
		c.logLevel = logging.Warn
	}))

	for range 100 {
		src.mustSendTx(t, src.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       &common.Address{},
			GasPrice: big.NewInt(100),
			Gas:      params.TxGas,
		}))
		src.syncMempool(t)

		vmTime.advance(850 * time.Millisecond)
		b := src.runConsensusLoop(t, src.lastAcceptedBlock(t))
		require.Len(t, b.Transactions(), 1, "transactions in block")
		require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()")

		t.Run("recover", func(t *testing.T) {
			newDB := memdb.New()
			it := srcDB.NewIterator()
			for it.Next() {
				require.NoError(t, newDB.Put(it.Key(), it.Value()))
			}
			require.NoError(t, it.Error())

			_, sut := newSUT(t, 3, sutOpt, options.Func[sutConfig](func(c *sutConfig) {
				c.db = newDB
				c.logLevel = logging.Warn
			}))

			t.Run("last", func(t *testing.T) {
				for name, fn := range map[string](func(vm *VM) *blocks.Block){
					"accepted": func(vm *VM) *blocks.Block { return vm.last.accepted.Load() },
					"executed": func(vm *VM) *blocks.Block { return vm.exec.LastExecuted() },
					"settled":  func(vm *VM) *blocks.Block { return vm.last.settled.Load() },
				} {
					t.Run(name, func(t *testing.T) {
						got := fn(sut.rawVM)
						want := fn(src.rawVM)
						if diff := cmp.Diff(want, got, blocks.CmpOpt()); diff != "" {
							t.Errorf("(-want +got):\n%s", diff)
						}
					})
				}
			})

			if diff := cmp.Diff(src.rawVM.blocks.m, sut.rawVM.blocks.m, blocks.CmpOpt()); diff != "" {
				t.Error(diff)
			}
		})
	}
}
