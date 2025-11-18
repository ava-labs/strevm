// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
)

var errExecutorClosed = errors.New("saexec.Executor closed")

// Enqueue pushes a new block to the FIFO queue. If [Executor.Close] is called
// before [blocks.Block.Executed] returns true then there is no guarantee that
// the block will be executed.
func (e *Executor) Enqueue(ctx context.Context, block *blocks.Block) error {
	warnAfter := time.Millisecond
	for {
		select {
		case e.queue <- block:
			return nil
		case <-e.quit:
			return errExecutorClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(warnAfter):
			// If this happens then increase the channel's buffer size.
			e.log.Warn(
				"Execution queue buffer too small",
				zap.Duration("wait", warnAfter),
				zap.Uint64("block_height", block.Height()),
			)
			warnAfter *= 2
		}
	}
}

func (e *Executor) processQueue() {
	defer close(e.done)

	for {
		select {
		case <-e.quit:
			return

		case block := <-e.queue:
			logger := e.log.With(
				zap.Uint64("block_height", block.Height()),
				zap.Uint64("block_time", block.BuildTime()),
				zap.Stringer("block_hash", block.Hash()),
				zap.Int("tx_count", len(block.Transactions())),
			)

			if err := e.execute(block, logger); err != nil {
				logger.Error("Block execution failed", zap.Error(err))
				return
			}
		}
	}
}

func (e *Executor) execute(b *blocks.Block, logger logging.Logger) error {
	logger.Debug("Executing block")

	// If the VM were to encounter an error after enqueuing the block, we would
	// receive the same block twice for execution should consensus retry
	// acceptance.
	if last, curr := e.lastExecuted.Load().Height(), b.Height(); curr != last+1 {
		return fmt.Errorf("executing blocks out of order: %d then %d", last, curr)
	}

	rules := e.chainConfig.Rules(b.Number(), true /*isMerge*/, b.BuildTime())

	// Since `b` hasn't been executed, it definitely hasn't been settled, so we
	// are guaranteed to have a non-nil parent available.
	parent := b.ParentBlock()

	stateDB, err := state.New(parent.PostExecutionStateRoot(), e.stateCache, e.snaps)
	if err != nil {
		return fmt.Errorf("state.New(%#x, ...): %v", parent.PostExecutionStateRoot(), err)
	}
	gasClock := parent.ExecutedByGasTime().Clone()

	if err := hook.BeforeBlock(e.hooks, rules, stateDB, b, gasClock); err != nil {
		return fmt.Errorf("before-block hook: %v", err)
	}
	perTxClock := gasClock.Time.Clone()

	header := types.CopyHeader(b.Header())
	header.BaseFee = gasClock.BaseFee().ToBig()

	gasPool := core.GasPool(math.MaxUint64) // required by geth but irrelevant so max it out
	var blockGasConsumed gas.Gas

	receipts := make(types.Receipts, len(b.Transactions()))
	for ti, tx := range b.Transactions() {
		stateDB.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			e.chainConfig,
			e.chainContext,
			&header.Coinbase,
			&gasPool,
			stateDB,
			header,
			tx,
			(*uint64)(&blockGasConsumed),
			vm.Config{},
		)
		if err != nil {
			// This almost certainly means that the worst-case block inclusion
			// has a bug.
			logger.Error(
				"Transaction execution errored (not reverted)",
				zap.Int("tx_index", ti),
				zap.Stringer("tx_hash", tx.Hash()),
				zap.Error(err),
			)
			continue
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
	hook.AfterBlock(e.hooks, stateDB, b.EthBlock(), gasClock, blockGasConsumed, receipts)
	if gasClock.Time.Compare(perTxClock) != 0 {
		return fmt.Errorf("broken invariant: block-resolution clock @ %s does not match tx-resolution clock @ %s", gasClock.String(), perTxClock.String())
	}

	logger.Debug(
		"Block execution complete",
		zap.Uint64("gas_consumed", uint64(blockGasConsumed)),
		zap.Time("gas_time", gasClock.AsTime()),
		zap.Time("wall_time", endTime),
	)

	root, err := stateDB.Commit(b.NumberU64(), true)
	if err != nil {
		return fmt.Errorf("%T.Commit() at end of block %d: %w", stateDB, b.NumberU64(), err)
	}
	// The strict ordering of the next 3 calls guarantees invariants that MUST
	// NOT be broken:
	//
	// 1. [blocks.Block.MarkExecuted] guarantees disk then in-memory changes.
	// 2. Internal indicator of last executed MUST follow in-memory change.
	// 3. External indicator of last executed MUST follow internal indicator.
	if err := b.MarkExecuted(e.db, gasClock.Clone(), endTime, header.BaseFee, receipts, root); err != nil {
		return err
	}
	e.lastExecuted.Store(b)                           // (2)
	e.sendPostExecutionEvents(b.EthBlock(), receipts) // (3)
	return nil
}
