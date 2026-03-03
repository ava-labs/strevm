// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saedb"
)

var errExecutorClosed = errors.New("saexec.Executor closed")

// BlockExecution holds the prepared state returned by [NewBlockExecution].
type BlockExecution struct {
	StateDB  *state.StateDB
	Header   *types.Header
	BlockCtx vm.BlockContext
	Signer   types.Signer
	GasPool  core.GasPool

	// Block, ChainConfig, and ChainContext are established once during
	// preparation and must not change across the execution lifecycle.
	Block        *types.Block
	ChainConfig  *params.ChainConfig
	ChainContext core.ChainContext

	// hooks is stored so that [BlockExecution.AfterExecutingBlock] uses the
	// same hooks as [hook.Points.BeforeExecutingBlock].
	hooks hook.Points
}

// ExecuteBlockConfig configures the shared block execution pipeline driven by
// [BlockExecution.ExecuteBlock]. Caller-specific behaviour is injected via
// optional callbacks.
type ExecuteBlockConfig struct {
	// Transactions to apply. The executor passes all block txs; the tracer
	// passes txs[:txIndex] to replay only preceding transactions.
	Transactions types.Transactions

	// Hooks for block lifecycle. If nil, [hook.Points.EndOfBlockOps] is
	// skipped entirely (which is the desired behaviour for state tracing).
	Hooks hook.Points

	// VMConfig is forwarded to [core.ApplyTransaction].
	VMConfig vm.Config

	// BeforeApplyingTx, if non-nil, is called before each transaction is applied.
	BeforeApplyingTx func(txIndex int, tx *types.Transaction)

	// AfterApplyingTx, if non-nil, is called after each transaction is
	// successfully applied.
	AfterApplyingTx func(txIndex int, tx *types.Transaction, receipt *types.Receipt)

	// BeforeApplyingOp, if non-nil, is called before each end-of-block
	// operation is applied.
	BeforeApplyingOp func(opIndexInBlock int, op hook.Op)

	// OnApplyTxError, if non-nil, is called when [core.ApplyTransaction]
	// returns an error.
	OnApplyTxError func(txIndex int, tx *types.Transaction, err error)

	// OnApplyOpError, if non-nil, is called when [hook.Op.ApplyTo] returns
	// an error.
	OnApplyOpError func(opIndex int, op hook.Op, err error)
}

// NewBlockExecution runs the pre-transaction phase of block execution,
// guaranteeing consistent ordering between normal execution and state tracing:
//
//  1. [hook.Points.BeforeExecutingBlock].
//  2. Header modification with the execution base fee.
//  3. Block context, signer, and gas pool creation.
func NewBlockExecution(
	stateDB *state.StateDB,
	block *types.Block,
	baseFee *big.Int,
	hooks hook.Points,
	chainConfig *params.ChainConfig,
	chainContext core.ChainContext,
) (*BlockExecution, error) {
	rules := chainConfig.Rules(block.Number(), true /*isMerge*/, block.Time())
	if err := hooks.BeforeExecutingBlock(rules, stateDB, block); err != nil {
		return nil, fmt.Errorf("before-block hook: %v", err)
	}

	header := types.CopyHeader(block.Header())
	header.BaseFee = baseFee

	return &BlockExecution{
		StateDB:      stateDB,
		Header:       header,
		BlockCtx:     core.NewEVMBlockContext(header, chainContext, &header.Coinbase),
		Signer:       types.MakeSigner(chainConfig, block.Number(), block.Time()),
		GasPool:      core.GasPool(math.MaxUint64),
		Block:        block,
		ChainConfig:  chainConfig,
		ChainContext: chainContext,
		hooks:        hooks,
	}, nil
}

// ExecuteBlock applies transactions and end-of-block operations to the
// prepared state. It does NOT call [BlockExecution.AfterExecutingBlock];
// callers MUST do so.
func (b *BlockExecution) ExecuteBlock(p *ExecuteBlockConfig) (types.Receipts, gas.Gas, error) {
	stateDB := b.StateDB

	var blockGasConsumed gas.Gas
	receipts := make(types.Receipts, len(p.Transactions))

	for ti, tx := range p.Transactions {
		stateDB.SetTxContext(tx.Hash(), ti)

		if p.BeforeApplyingTx != nil {
			p.BeforeApplyingTx(ti, tx)
		}

		receipt, err := core.ApplyTransaction(
			b.ChainConfig,
			b.ChainContext,
			&b.Header.Coinbase,
			&b.GasPool,
			stateDB,
			b.Header,
			tx,
			(*uint64)(&blockGasConsumed),
			p.VMConfig,
		)
		if err != nil {
			if p.OnApplyTxError != nil {
				p.OnApplyTxError(ti, tx, err)
			}
			return nil, 0, fmt.Errorf("applying tx %d (%v): %w", ti, tx.Hash(), err)
		}

		receipts[ti] = receipt

		if p.AfterApplyingTx != nil {
			p.AfterApplyingTx(ti, tx, receipt)
		}
	}

	if p.Hooks != nil {
		numTxs := len(p.Transactions)
		for i, o := range p.Hooks.EndOfBlockOps(b.Block) {
			blockGasConsumed += o.Gas

			if p.BeforeApplyingOp != nil {
				p.BeforeApplyingOp(numTxs+i, o)
			}

			if err := o.ApplyTo(stateDB); err != nil {
				if p.OnApplyOpError != nil {
					p.OnApplyOpError(i, o, err)
				}
				return nil, 0, fmt.Errorf("end-of-block op %d: %w", i, err)
			}
		}
	}

	return receipts, blockGasConsumed, nil
}

// AfterExecutingBlock completes the block lifecycle started by
// [NewBlockExecution]. Callers MUST invoke this after
// [BlockExecution.ExecuteBlock] returns.
func (b *BlockExecution) AfterExecutingBlock(receipts types.Receipts) {
	b.hooks.AfterExecutingBlock(b.StateDB, b.Block, receipts)
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
				logger.Fatal(
					"Block execution failed; see emergency playbook",
					zap.Error(err),
					zap.String("playbook", "https://github.com/ava-labs/strevm/issues/28"),
				)
				return
			}
		}
	}
}

func (e *Executor) execute(b *blocks.Block, logger logging.Logger) error {
	logger.Debug("Executing block")

	// Since `b` hasn't been executed, it definitely hasn't been settled, so we
	// are guaranteed to have a non-nil parent available.
	parent := b.ParentBlock()
	// If the VM were to encounter an error after enqueuing the block, we would
	// receive the same block twice for execution should consensus retry
	// acceptance.
	if last := e.lastExecuted.Load().Hash(); last != parent.Hash() {
		return fmt.Errorf("executing block built on parent %#x when last executed %#x", parent.Hash(), last)
	}

	stateDB, err := state.New(parent.PostExecutionStateRoot(), e.stateCache, e.snaps)
	if err != nil {
		return fmt.Errorf("state.New(%#x, ...): %v", parent.PostExecutionStateRoot(), err)
	}

	gasClock := parent.ExecutedByGasTime().Clone()
	gasClock.BeforeBlock(e.hooks, b.Header())
	perTxClock := gasClock.Time.Clone()

	baseFee := gasClock.BaseFee()
	b.CheckBaseFeeBound(baseFee)

	prep, err := NewBlockExecution(stateDB, b.EthBlock(), baseFee.ToBig(), e.hooks, e.chainConfig, e.chainContext)
	if err != nil {
		return err
	}

	receipts, blockGasConsumed, err := prep.ExecuteBlock(&ExecuteBlockConfig{
		Transactions: b.Transactions(),
		Hooks:        e.hooks,

		BeforeApplyingTx: func(ti int, tx *types.Transaction) {
			b.CheckSenderBalanceBound(stateDB, prep.Signer, tx)
			logger = logger.With(
				zap.Int("tx_index", ti),
				zap.Stringer("tx_hash", tx.Hash()),
			)
		},

		AfterApplyingTx: func(ti int, tx *types.Transaction, receipt *types.Receipt) {
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
			tip := tx.EffectiveGasTipValue(prep.Header.BaseFee)
			receipt.EffectiveGasPrice = tip.Add(prep.Header.BaseFee, tip)

			// Even though we populated the value ourselves and `ok == true` is
			// guaranteed when using the [Executor] via the public API, it's clearer
			// to check than to require the reader to reason about dropping the
			// flag.
			if ch, ok := e.receipts.Load(tx.Hash()); ok {
				ch <- &Receipt{receipt, prep.Signer, tx}
			}
		},

		BeforeApplyingOp: func(opIndexInBlock int, op hook.Op) {
			b.CheckOpBurnerBalanceBounds(stateDB, opIndexInBlock, op)
			perTxClock.Tick(op.Gas)
			b.SetInterimExecutionTime(perTxClock)
		},

		OnApplyTxError: func(ti int, tx *types.Transaction, err error) {
			logger.Fatal(
				"Transaction execution errored (not reverted); see emergency playbook",
				zap.String("playbook", "https://github.com/ava-labs/strevm/issues/28"),
				zap.Error(err),
			)
		},

		OnApplyOpError: func(i int, op hook.Op, err error) {
			logger.Fatal(
				"Extra block operation errored; see emergency playbook",
				zap.Int("op_index", i),
				zap.Stringer("op_id", op.ID),
				zap.String("playbook", "https://github.com/ava-labs/strevm/issues/28"),
				zap.Error(err),
			)
		},
	})
	if err != nil {
		return err
	}

	prep.AfterExecutingBlock(receipts)
	endTime := time.Now()
	if err := gasClock.AfterBlock(blockGasConsumed, e.hooks, b.Header()); err != nil {
		return fmt.Errorf("after-block gas time update: %w", err)
	}

	logger.Debug(
		"Block execution complete",
		zap.Uint64("gas_consumed", uint64(blockGasConsumed)),
		zap.Time("gas_time", gasClock.AsTime()),
		zap.Time("wall_time", endTime),
	)

	e.chainContext.recent.Put(b.NumberU64(), b.Header())

	root, err := stateDB.Commit(b.NumberU64(), true)
	if err != nil {
		return fmt.Errorf("%T.Commit() at end of block %d: %w", stateDB, b.NumberU64(), err)
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
	if err := b.MarkExecuted(e.db, e.xdb, gasClock.Clone(), endTime, prep.Header.BaseFee, receipts, root, &e.lastExecuted /* (2) */); err != nil {
		return err
	}
	e.sendPostExecutionEvents(b.EthBlock(), receipts) // (3)
	return nil
}
