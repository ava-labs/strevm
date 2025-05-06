//go:build dev

package sae

import (
	"context"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"

	_ "embed"
)

//go:embed tx0.bin
var tx0 []byte

func (vm *VM) afterInitialize(ctx context.Context, toEngine chan<- common.Message) error {
	vm.logger().Info("##### Populating mempool")
	tx := new(types.Transaction)
	if err := rlp.DecodeBytes(tx0, tx); err != nil {
		return err
	}
	if err := vm.builder.mempool.Use(ctx, 0, func(_ <-chan sink.Priority, mp *mempool) error {
		mp.pool.Push(&transaction{
			tx:   tx,
			from: ethcommon.HexToAddress("0x9cce34f7ab185c7aba1b7c8140d620b4bda941d6"),
		})
		return nil
	}); err != nil {
		return err
	}

	go func() {
		time.Sleep(10 * time.Second)
		vm.logger().Info("##### Signalling pending txs")
		toEngine <- common.PendingTxs
		vm.builder.mempool.Use(context.Background(), 0, func(_ <-chan sink.Priority, mp *mempool) error {
			vm.logger().Info(
				"##### Mempool has pending txs",
				zap.Int("length", mp.pool.Len()),
			)
			return nil
		})
	}()

	return nil
}
