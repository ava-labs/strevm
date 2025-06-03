package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/queue"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

func (vm *VM) buildBlock(ctx context.Context, timestamp uint64, parent *Block) (*Block, error) {
	block, err := sink.FromPriorityMutex(
		ctx, vm.mempool, sink.MaxPriority,
		func(_ <-chan sink.Priority, pool *queue.Priority[*pendingTx]) (*Block, error) {
			return vm.buildBlockWithCandidateTxs(timestamp, parent, pool)
		},
	)
	if err != nil {
		return nil, err
	}
	vm.logger().Debug(
		"Built block",
		zap.Uint64("timestamp", timestamp),
		zap.Uint64("height", block.Height()),
		zap.Stringer("parent", parent.Hash()),
	)
	return block, nil
}

var errWaitingForExecution = errors.New("waiting for execution when building block")

func (vm *VM) buildBlockWithCandidateTxs(timestamp uint64, parent *Block, candidateTxs queue.Queue[*pendingTx]) (*Block, error) {
	if timestamp < parent.Time() {
		return nil, fmt.Errorf("block at time %d before parent at %d", timestamp, parent.Time())
	}

	toSettle, ok := vm.lastBlockToSettleAt(timestamp, parent)
	if !ok {
		vm.logger().Warn(
			"Block building waiting for execution",
			zap.Uint64("timestamp", timestamp),
			zap.Stringer("parent", parent.Hash()),
		)
		return nil, fmt.Errorf("%w: parent %#x at time %d", errWaitingForExecution, parent.Hash(), timestamp)
	}
	vm.logger().Debug(
		"Settlement candidate",
		zap.Uint64("timestamp", timestamp),
		zap.Stringer("parent", parent.Hash()),
		zap.Stringer("block_hash", toSettle.Hash()),
		zap.Uint64("block_height", toSettle.Height()),
		zap.Uint64("block_time", toSettle.Time()),
	)

	var (
		receipts []types.Receipts
		gasUsed  uint64
	)
	for _, b := range settling(parent.lastSettled, toSettle) {
		receipts = append(receipts, b.execution.receipts)
		for _, r := range b.execution.receipts {
			gasUsed += r.GasUsed
		}
	}

	txs, gasLimit, err := vm.buildBlockOnHistory(toSettle, parent, timestamp, candidateTxs)
	if err != nil {
		return nil, err
	}

	b := vm.newBlock(types.NewBlock(
		&types.Header{
			ParentHash: parent.Hash(),
			Root:       toSettle.execution.stateRootPost,
			Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
			GasLimit:   gasLimit,
			GasUsed:    gasUsed,
			Time:       timestamp,
			BaseFee:    nil, // TODO(arr4n)
		},
		txs, nil, /*uncles*/
		slices.Concat(receipts...),
		trieHasher(),
	))
	b.parent = parent
	b.lastSettled = toSettle
	return b, nil
}

func (vm *VM) buildBlockOnHistory(lastSettled, parent *Block, timestamp uint64, candidateTxs queue.Queue[*pendingTx]) (types.Transactions, uint64, error) {
	var history []*Block
	for b := parent; b.ID() != lastSettled.ID(); b = b.parent {
		history = append(history, b)
	}
	slices.Reverse(history)

	sdb, err := state.New(lastSettled.execution.stateRootPost, vm.exec.stateCache, nil)
	if err != nil {
		return nil, 0, err
	}

	blockNum := parent.NumberU64() + 1
	signer := vm.signer(blockNum, timestamp)
	checker := validityChecker{
		db:  sdb,
		log: vm.logger(),
		rules: vm.exec.chainConfig.Rules(
			new(big.Int).SetUint64(blockNum),
			true,
			timestamp,
		),
		gasClock: lastSettled.execution.by.clone(),
		nonces:   make(map[common.Address]uint64),
		balances: make(map[common.Address]*uint256.Int),
	}
	clock := &checker.gasClock

	// TODO(arr4n): investigate caching values to avoid having to replay the
	// entire history.
	for _, b := range history {
		clock.fastForward(b.Time())

		var consumed gas.Gas
		for _, tx := range b.Transactions() {
			from, err := types.Sender(signer, tx)
			if err != nil {
				return nil, 0, err
			}

			valid, err := checker.addTxToQueue(txAndSender{tx, from})
			if err != nil || valid != includeTx {
				vm.logger().Error(
					"Invalid transaction when replaying history",
					zap.Stringer("block", b.Hash()),
					zap.Stringer("tx", tx.Hash()),
					zap.Error(err),
					zap.Stringer("validity", valid),
				)
				return nil, 0, err
			}
			consumed += gas.Gas(tx.Gas())
		}
		clock.consume(consumed)
	}

	var (
		txs types.Transactions
		// TODO(arr4n) setting `gasUsed` (i.e. historical) based on receipts and
		// `gasLimit` (future-looking) based on the enqueued transactions
		// follows naturally from all of the other changes. However it will be
		// possible to have `gasUsed>gasLimit`, which may break some systems.
		gasLimit uint64
		delayed  []*pendingTx
	)

	clock.fastForward(timestamp)
TxLoop:
	for candidateTxs.Len() > 0 {
		candidate := candidateTxs.Pop()
		tx := candidate.tx

		validity, err := checker.addTxToQueue(candidate.txAndSender)
		if err != nil {
			return nil, 0, err
		}

		switch validity {
		case includeTx:
			// TODO(arr4n) parameterise the max block and queue sizes.
			if gl := gasLimit + tx.Gas(); gl <= uint64(maxGasPerSecond*maxGasSecondsPerBlock) {
				gasLimit = gl
				txs = append(txs, tx)
			} else {
				delayed = append(delayed, candidate)
				break TxLoop
			}

		case delayTx, queueFull:
			delayed = append(delayed, candidate)
			vm.logger().Debug(
				"Delaying transaction until later block-building",
				zap.Stringer("hash", tx.Hash()),
			)
			if validity == queueFull {
				break TxLoop
			}

		case discardTx:
			vm.logger().Debug(
				"Discarding transaction",
				zap.Stringer("hash", tx.Hash()),
			)

		}
	}
	for _, tx := range delayed {
		candidateTxs.Push(tx)
	}

	return txs, gasLimit, nil
}

func (vm *VM) lastBlockToSettleAt(timestamp uint64, parent *Block) (*Block, bool) {
	// These variables are only abstracted for clarity; they are not needed
	// beyond the scope of the `for` loop.
	var block, child *Block
	block = parent // therefore `child` remains nil
	settleAt := boundedSubtract(timestamp, stateRootDelaySeconds, vm.last.synchronousTime)

	// The only way [Block.parent] can be nil is if it was already settled (see
	// invariant in [Block]). If a block was already settled then only that or a
	// later (i.e. unsettled) block can be returned by this loop, therefore we
	// have a guarantee that the loop update will never result in `block==nil`.
	// Framed differently, because `settleAt` is >= what it was when this
	// function was used to build `parent`, if `block.parent==nil` then it will
	// have already been settled in `parent` and therefore executed by
	// `<=settleAt` so will be returned here.
	for ; ; block, child = block.parent, block {
		if block.Time() > settleAt {
			continue
		}

		if block.executed.Load() {
			if block.execution.by.after(settleAt) {
				continue
			}
			return block, true
		}

		// TODO(arr4n) more fine-grained checks are possible for scenarios where
		// (a) `block` could never execute before `settleAt` so we would
		// `continue`; and (b) `block` will definitely execute in time and
		// `child` could never, in which case return `nil, false`.
		_ = child

		return nil, false
	}
}

func (b *Block) mightFitWithinGasLimit(max gas.Gas) bool {
	for _, tx := range b.Transactions() {
		g := minGasCharged(tx)
		if g > max {
			return false
		}
		max -= g
	}
	return true
}

func minGasCharged(tx *types.Transaction) gas.Gas {
	return gas.Gas(tx.Gas()) >> 1
}
