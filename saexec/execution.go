package saexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/dummy"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/queue"
)

// Enqueue pushes a new block to the FIFO queue.
func (e *Executor) EnqueueAccepted(ctx context.Context, block *blocks.Block) error {
	return e.queue.UseThenSignal(ctx, func(q *queue.FIFO[*blocks.Block]) error {
		q.Push(block)
		return nil
	})
}

func (e *Executor) processQueue() {
	ctx := e.quitCtx()

	for {
		block, err := sink.FromMonitor(ctx, e.queue,
			func(q *queue.FIFO[*blocks.Block]) bool {
				return q.Len() > 0
			},
			func(q *queue.FIFO[*blocks.Block]) (*blocks.Block, error) {
				return q.Pop(), nil
			},
		)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			// [sink.Monitor.Wait] will only return the [context.Context] error
			// or the error returned by its argument, so this is theoretically
			// impossible but included for completeness to be detected in tests.
			e.log.Fatal("BUG: popping from queue", zap.Error(err))
			return
		}

		switch err := e.execute(ctx, block); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			e.log.Fatal(
				"Executing accepted block",
				zap.Error(err),
				zap.Uint64("height", block.Height()),
				zap.Uint64("timestamp", block.Time()),
				zap.Any("hash", block.Hash()),
			)
			return
		}
	}
}

type executionScratchSpace struct {
	snaps   *snapshot.Tree
	statedb *state.StateDB
}

func (e *Executor) execute(ctx context.Context, b *blocks.Block) error {
	x := &e.executeScratchSpace

	// If [VM.AcceptBlock] returns an error after enqueuing the block, we would
	// receive the same block twice for execution should consensus retry
	// acceptance.
	if last, curr := e.lastExecuted.Load().Height(), b.Height(); curr != last+1 {
		return fmt.Errorf("executing blocks out of order: %d then %d", last, curr)
	}

	hook.BeforeBlock(&e.gasClock, b.Header(), e.hooks.GasTarget(b.ParentBlock().Block))
	perTxClock := e.gasClock.Time.Clone()

	header := types.CopyHeader(b.Header())
	header.BaseFee = e.gasClock.BaseFee().ToBig()
	e.log.Debug(
		"Executing accepted block",
		zap.Uint64("height", b.Height()),
		zap.Uint64("timestamp", header.Time),
		zap.Int("transactions", len(b.Transactions())),
	)

	gasPool := core.GasPool(math.MaxUint64) // required by geth but irrelevant so max it out
	var blockGasConsumed gas.Gas

	receipts := make(types.Receipts, len(b.Transactions()))
	for ti, tx := range b.Transactions() {
		x.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			e.chainConfig,
			dummy.ChainContext(),
			&header.Coinbase,
			&gasPool,
			x.statedb,
			header,
			tx,
			(*uint64)(&blockGasConsumed),
			vm.Config{},
		)
		if err != nil {
			return fmt.Errorf("tx[%d]: %w", ti, err)
		}

		perTxClock.Tick(gas.Gas(receipt.GasUsed))
		b.SetInterimExecutionTime(perTxClock)
		// TODO(arr4n) investigate calling the same method on pending blocks in
		// the queue. It's only worth it if [blocks.LastToSettleAt] regularly
		// returns false, meaning that execution is blocking consensus.

		// The [types.Header] that we pass to [core.ApplyTransaction] is
		// modified to reduce gas price from the worst-case value agreed by
		// consensus. This changes the hash, which is what is copied to receipts
		// and logs.
		receipt.BlockHash = b.Hash()
		for _, l := range receipt.Logs {
			l.BlockHash = b.Hash()
		}

		// TODO(arr4n) add a receipt cache to the [executor] to allow API calls
		// to access them before the end of the block.
		receipts[ti] = receipt
	}
	endTime := time.Now()
	hook.AfterBlock(&e.gasClock, blockGasConsumed)
	if e.gasClock.Time.Cmp(perTxClock) != 0 {
		return fmt.Errorf("broken invariant: block-resolution clock @ %s does not match tx-resolution clock @ %s", e.gasClock.String(), perTxClock.String())
	}

	root, err := e.commitState(ctx, x, b.NumberU64())
	if err != nil {
		return err
	}
	// The strict ordering of the next 3 calls guarantees invariants that MUST
	// NOT be broken:
	//
	// 1. [blocks.Block.MarkExecuted] guarantees disk then in-memory changes.
	// 2. Internal indicator of last executed MUST follow in-memory change.
	// 3. External indicator of last executed MUST follow internal indicator.
	if err := b.MarkExecuted(e.db, e.gasClock.Clone(), endTime, receipts, root); err != nil {
		return err
	}
	e.lastExecuted.Store(b)                      // (2)
	e.sendPostExecutionEvents(b.Block, receipts) // (3)

	e.log.Debug(
		"Block execution complete",
		zap.Uint64("height", b.Height()),
		zap.Time("gas_time", e.gasClock.AsTime()),
		zap.Time("wall_time", endTime),
		zap.Int("tx_count", len(b.Transactions())),
	)
	return nil
}

func (e *Executor) commitState(ctx context.Context, x *executionScratchSpace, blockNum uint64) (common.Hash, error) {
	root, err := x.statedb.Commit(blockNum, true)
	if err != nil {
		return common.Hash{}, fmt.Errorf("%T.Commit() at end of block %d: %w", x.statedb, blockNum, err)
	}

	db, err := state.New(root, e.stateCache, x.snaps)
	if err != nil {
		return common.Hash{}, err
	}
	x.statedb = db
	return root, nil
}
