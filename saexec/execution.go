// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
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
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saedb"
)

var errExecutorClosed = errors.New("saexec.Executor closed")

type (
	// A StateDBOpener opens a [state.StateDB] at the given root. The
	// [Executor] implements this interface.
	StateDBOpener interface {
		StateDB(root common.Hash) (*state.StateDB, error)
	}

	// A ReceiptStore receives per-transaction receipts during block execution.
	// The [Executor] implementation broadcasts to waiting RPC callers and the
	// tracer uses [NullReceiptStore].
	ReceiptStore interface {
		Load(common.Hash) (chan *Receipt, bool)
	}

	// NoEndOfBlockOps wraps [hook.Points] to suppress
	// [hook.Points.EndOfBlockOps], used by the tracer to skip end-of-block
	// operations during partial replay.
	NoEndOfBlockOps struct {
		hook.Points
	}

	// ExecutionResults holds the outputs of [Execute].
	ExecutionResults struct {
		GasConsumed gas.Gas
		StateDB     *state.StateDB
		Signer      types.Signer
		Header      *types.Header
		BlockCtx    vm.BlockContext
		Receipts    types.Receipts
		FinishBy    struct {
			Gas  *gastime.Time
			Wall time.Time
		}
	}
)

// EndOfBlockOps always returns nil.
func (NoEndOfBlockOps) EndOfBlockOps(*types.Block) ([]hook.Op, error) { return nil, nil }

// Execute is the shared execution pipeline for both [Executor.execute] and
// the state-tracing replay path. Bound checks and interim execution time
// updates are called unconditionally - they are safe no-ops when b has nil
// bounds (e.g. restored settled blocks). maxNumTxs limits the number of
// transactions to process. The tracer uses this to replay only preceding
// transactions.
func Execute(
	b *blocks.Block,
	stateDB *state.StateDB,
	parentGasTime *gastime.Time,
	maxNumTxs int,
	hooks hook.Points,
	config *params.ChainConfig,
	chainCtx core.ChainContext,
	receiptStore ReceiptStore,
	log logging.Logger,
) (*ExecutionResults, error) {
	log.Debug("Executing block")

	gasClock := parentGasTime.Clone()
	gasClock.BeforeBlock(hooks, b.Header())
	perTxClock := gasClock.Time.Clone()

	ethBlock := b.EthBlock()
	rules := config.Rules(ethBlock.Number(), true /*isMerge*/, ethBlock.Time())
	if err := hooks.BeforeExecutingBlock(rules, stateDB, ethBlock); err != nil {
		return nil, fmt.Errorf("before-block hook: %v", err)
	}

	baseFee := gasClock.BaseFee()
	b.CheckBaseFeeBound(baseFee)
	header := types.CopyHeader(ethBlock.Header())
	header.BaseFee = baseFee.ToBig()

	blockCtx := core.NewEVMBlockContext(header, chainCtx, &header.Coinbase)
	signer := types.MakeSigner(config, ethBlock.Number(), ethBlock.Time())
	gasPool := core.GasPool(math.MaxUint64)
	var blockGasConsumed gas.Gas

	txs := b.Transactions()
	txs = txs[:min(len(txs), maxNumTxs)]
	allReceipts := make(types.Receipts, len(txs))

	for ti, tx := range txs {
		stateDB.SetTxContext(tx.Hash(), ti)
		b.CheckSenderBalanceBound(stateDB, signer, tx)

		log = log.With(
			zap.Int("tx_index", ti),
			zap.Stringer("tx_hash", tx.Hash()),
		)

		receipt, err := core.ApplyTransaction(
			config,
			chainCtx,
			&header.Coinbase,
			&gasPool,
			stateDB,
			header,
			tx,
			(*uint64)(&blockGasConsumed),
			vm.Config{},
		)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction execution errored (not reverted) [%d](%#x): %v", errFatal, ti, tx.Hash(), err)
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
		//
		// [core.ApplyTransaction] also doesn't set [types.Receipt.EffectiveGasPrice].
		// Fixing both here avoids needing to call [types.Receipt.DeriveFields].
		receipt.BlockHash = b.Hash()
		for _, l := range receipt.Logs {
			l.BlockHash = b.Hash()
		}
		tip := tx.EffectiveGasTipValue(header.BaseFee)
		receipt.EffectiveGasPrice = tip.Add(header.BaseFee, tip)

		if ch, ok := receiptStore.Load(tx.Hash()); ok {
			ch <- &Receipt{receipt, signer, tx}
		}
		allReceipts[ti] = receipt
	}

	numTxs := len(b.Transactions())
	ops, err := hooks.EndOfBlockOps(ethBlock)
	if err != nil {
		return nil, fmt.Errorf("%w: %T.EndOfBlockOps(%#x): %v", errFatal, hooks, b.Hash(), err)
	}
	for i, o := range ops {
		b.CheckOpBurnerBalanceBounds(stateDB, numTxs+i, o)
		blockGasConsumed += o.Gas
		perTxClock.Tick(o.Gas)
		b.SetInterimExecutionTime(perTxClock)

		if err := o.ApplyTo(stateDB); err != nil {
			return nil, fmt.Errorf("%w: applying end-of-block operation [%d](%v): %v", errFatal, i, o.ID, err)
		}
	}

	hooks.AfterExecutingBlock(stateDB, ethBlock, allReceipts)
	endTime := time.Now()
	if err := gasClock.AfterBlock(blockGasConsumed, hooks, b.Header()); err != nil {
		return nil, fmt.Errorf("after-block gas time update: %w", err)
	}

	log.Debug(
		"Block execution complete",
		zap.Uint64("gas_consumed", uint64(blockGasConsumed)),
		zap.Time("gas_time", gasClock.AsTime()),
		zap.Time("wall_time", endTime),
	)

	r := &ExecutionResults{
		GasConsumed: blockGasConsumed,
		StateDB:     stateDB,
		Signer:      signer,
		Header:      header,
		BlockCtx:    blockCtx,
		Receipts:    allReceipts,
	}
	r.FinishBy.Gas = gasClock
	r.FinishBy.Wall = endTime
	return r, nil
}

// Enqueue pushes a new block to the FIFO queue. If [Executor.Close] is called
// before [blocks.Block.Executed] returns true then there is no guarantee that
// the block will be executed.
func (e *Executor) Enqueue(ctx context.Context, block *blocks.Block) error {
	e.createReceiptBuffers(block)
	select {
	case e.queue <- block:
		if n := len(e.queue); n == cap(e.queue) {
			// If this happens then increase the channel's buffer size.
			e.log.Warn(
				"Execution queue buffer full",
				zap.Uint64("block_height", block.Height()),
				zap.Int("queue_capacity", n),
			)
		}
		return nil

	case <-ctx.Done():
		return ctx.Err()
	case <-e.quit:
		return errExecutorClosed
	case <-e.done:
		// `e.done` can also close due to [Executor.execute] errors.
		return errExecutorClosed
	}
}

const emergencyPlaybookLink = "https://github.com/ava-labs/strevm/issues/28"

func (e *Executor) processQueue() {
	defer close(e.done)

	for {
		select {
		case <-e.quit:
			return

		case block := <-e.queue:
			log := e.log.With(
				zap.Uint64("block_height", block.Height()),
				zap.Uint64("block_time", block.BuildTime()),
				zap.Stringer("block_hash", block.Hash()),
				zap.Int("tx_count", len(block.Transactions())),
			)

			err := e.execute(block, log)
			switch {
			case errors.Is(err, errFatal):
				log.Fatal( //nolint:gocritic // False positive, will not terminate the process
					"Block execution failed",
					zap.String("playbook", emergencyPlaybookLink),
					zap.Error(err),
				)
			case err != nil:
				log.Error(
					"Error of unknown severity in block execution",
					zap.String("if_escalation_required", emergencyPlaybookLink),
					zap.Error(err),
				)
			}
			if err != nil {
				return
			}
		}
	}
}

var errFatal = errors.New("fatal execution error")

func (e *Executor) execute(b *blocks.Block, log logging.Logger) error {
	// Since `b` hasn't been executed, it definitely hasn't been settled, so we
	// are guaranteed to have a non-nil parent available.
	parent := b.ParentBlock()
	// If the VM were to encounter an error after enqueuing the block, we would
	// receive the same block twice for execution should consensus retry
	// acceptance.
	if last := e.lastExecuted.Load().Hash(); last != parent.Hash() {
		return fmt.Errorf("executing block built on parent %#x when last executed %#x", parent.Hash(), last)
	}

	stateDB, err := e.StateDB(parent.PostExecutionStateRoot())
	if err != nil {
		return fmt.Errorf("state.New(%#x, ...): %v", parent.PostExecutionStateRoot(), err)
	}

	result, err := Execute(b, stateDB, parent.ExecutedByGasTime(), math.MaxInt, e.hooks, e.chainConfig, e.chainContext, e.receipts, log)
	if err != nil {
		return err
	}

	return e.afterExecution(b, result)
}

func (e *Executor) afterExecution(b *blocks.Block, r *ExecutionResults) error {
	e.chainContext.recent.Put(b.NumberU64(), b.Header())

	root, err := r.StateDB.Commit(b.NumberU64(), true)
	if err != nil {
		return fmt.Errorf("%T.Commit() at end of block %d: %w", r.StateDB, b.NumberU64(), err)
	}
	if num := b.NumberU64(); saedb.ShouldCommitTrieDB(num) {
		tdb := e.stateCache.TrieDB()
		if err := tdb.Commit(root, false /* log */); err != nil {
			return fmt.Errorf("%T.Commit(%#x) at end of block %d: %v", tdb, root, num, err)
		}
	}
	// The strict ordering of the next 3 calls guarantees invariants that MUST
	// NOT be broken:
	//
	// 1. [blocks.Block.MarkExecuted] guarantees disk then in-memory changes.
	// 2. Internal indicator of last executed MUST follow in-memory change.
	// 3. External indicator of last executed MUST follow internal indicator.
	if err := b.MarkExecuted(e.db, e.xdb, r.FinishBy.Gas.Clone(), r.FinishBy.Wall, r.Header.BaseFee, r.Receipts, root, &e.lastExecuted /* (2) */); err != nil {
		return err
	}
	e.sendPostExecutionEvents(b.EthBlock(), r.Receipts) // (3)
	return nil
}

// NullReceiptStore is a no-op [ReceiptStore] for use when receipt broadcasting
// is not needed (e.g. state tracing).
type NullReceiptStore struct{}

var _ ReceiptStore = (*NullReceiptStore)(nil)

func (*NullReceiptStore) Load(common.Hash) (chan *Receipt, bool) {
	return nil, false
}
