package sae

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arr4n/sink"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

var errReceivedQuitSig = errors.New("quit")

func (vm *VM) startMempool() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-vm.quit
		cancel()
	}()

	for {
		err := vm.mempool.Use(ctx, sink.MinPriority, vm.receiveTxs)
		if errors.Is(err, errReceivedQuitSig) {
			return
		}
		if err != nil {
			vm.logger().Error("Accepting to mempool", zap.Error(err))
		}
		// err == nil -> wait for high-priority mempool user to finish
		_ = 0 // visual coverage aid
	}
}

func (vm *VM) receiveTxs(preempt <-chan sink.Priority, pool *queue.Priority[*pendingTx]) error {
	signer := vm.signer()
	for {
		select {
		case <-vm.quit:
			return errReceivedQuitSig
		case <-preempt:
			return nil
		case tx, ok := <-vm.newTxs:
			if !ok {
				return fmt.Errorf("new-transaction channel closed unexpectedly")
			}

			from, err := types.Sender(signer, tx)
			if err != nil {
				vm.logger().Debug(
					"Dropped tx due to failed sender recovery",
					zap.Stringer("hash", tx.Hash()),
					zap.Error(err),
				)
				continue
			}
			pool.Push(&pendingTx{
				txAndSender: txAndSender{
					tx:   tx,
					from: from,
				},
				timePriority: time.Now(),
			})
			vm.logger().Debug(
				"New tx in mempool",
				zap.Stringer("hash", tx.Hash()),
				zap.Stringer("from", from),
				zap.Uint64("nonce", tx.Nonce()),
			)

			select {
			case vm.toEngine <- snowcommon.PendingTxs:
			default:
				p := snowcommon.PendingTxs
				vm.logger().Debug(fmt.Sprintf("%T(%s) dropped", p, p))
			}
		}
	}
}

type txAndSender struct {
	tx   *types.Transaction
	from common.Address
}

type pendingTx struct {
	txAndSender

	// timePriority is initially the time at which the tx was first seen, and is
	// updated to the current time whenever the tx is placed back in the queue
	// for a later retry.
	timePriority  time.Time
	retryAttempts uint
}

func (tx *pendingTx) LessThan(u *pendingTx) bool {
	if tx.from != u.from {
		return tx.timePriority.Before(u.timePriority)
	}

	if nt, nu := tx.tx.Nonce(), u.tx.Nonce(); nt != nu {
		return nt < nu
	}

	switch tx.tx.GasFeeCap().Cmp(u.tx.GasFeeCap()) {
	case -1:
		return true
	case 1:
		return false
	default: // 0 but the compiler doesn't know this is the only option (yeah, yeah, Rust...)
		return tx.timePriority.Before(u.timePriority)
	}
}
