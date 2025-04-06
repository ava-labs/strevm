package sae

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

func (bb *blockBuilder) startMempool() {
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-bb.quit
			cancel()
		}()
		bb.acceptToMempool(ctx)
		bb.done <- struct{}{}
	}()
}

func (bb *blockBuilder) acceptToMempool(ctx context.Context) {
	for {
		err := bb.mempool.Use(ctx, 0, func(preempt <-chan sink.Priority, mp *mempool) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-preempt:
					return nil
				case tx, ok := <-bb.newTxs:
					if !ok {
						return fmt.Errorf("new-transaction channel closed unexpectedly")
					}
					mp.pool.Push(tx)
				}
			}
		})

		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			bb.log.Error("Accepting to mempool", zap.Error(err))
		}
		// err == nil -> wait for high-priority mempool user to finish
		_ = 0 // visual coverage aid
	}
}

type mempool struct {
	pool queue.Priority[*transaction]
}

type transaction struct {
	tx   *types.Transaction
	from common.Address

	// timePriority is initially the time at which the tx was first seen, and is
	// updated to the current time whenever the tx is placed back in the queue
	// for a later retry.
	timePriority  time.Time
	retryAttempts uint
}

func (tx *transaction) LessThan(u *transaction) bool {
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
