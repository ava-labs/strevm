//go:build dev

package sae

import (
	"context"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/queue"
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
	vm.newTxs <- tx

	go func() {
		time.Sleep(10 * time.Second)
		vm.logger().Info("##### Signalling pending txs")
		toEngine <- common.PendingTxs
		vm.mempool.Use(ctx, 0, func(_ <-chan sink.Priority, mp *queue.Priority[*pendingTx]) error {
			vm.logger().Info(
				"##### Mempool has pending txs",
				zap.Int("length", mp.Len()),
			)
			return nil
		})
	}()

	return nil
}
