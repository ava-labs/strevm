package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/queue"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

func (vm *VM) buildBlock(ctx context.Context, timestamp uint64, parent *Block, db ethdb.Database) (*Block, error) {
	// TODO(arr4n) implement sink.FromPriorityMutex()
	var block *Block
	if err := vm.mempool.Use(ctx, sink.Priority(math.MaxUint64), func(preempt <-chan sink.Priority, pool *queue.Priority[*pendingTx]) error {
		var err error
		block, err = vm.buildBlockWithCandidateTxs(timestamp, parent, db, pool)
		return err
	}); err != nil {
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

func (vm *VM) buildBlockWithCandidateTxs(timestamp uint64, parent *Block, db ethdb.Database, candidateTxs queue.Queue[*pendingTx]) (*Block, error) {
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
	for b := toSettle; b.parent != nil && b.ID() != parent.lastSettled.ID(); b = b.parent {
		receipts = append(receipts, b.execution.receipts)
		for _, r := range b.execution.receipts {
			gasUsed += r.GasUsed
		}
	}
	slices.Reverse(receipts)

	txs, gasLimit, err := vm.buildBlockOnHistory(toSettle, parent, db, candidateTxs)
	if err != nil {
		return nil, err
	}

	return &Block{
		Block: types.NewBlock(
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
			trie.NewStackTrie(nil),
		),
		parent:      parent,
		lastSettled: toSettle,
	}, nil
}

func (vm *VM) buildBlockOnHistory(lastSettled, parent *Block, db ethdb.Database, candidateTxs queue.Queue[*pendingTx]) (types.Transactions, uint64, error) {
	var history []*Block
	for b := parent; b.ID() != lastSettled.ID(); b = b.parent {
		history = append(history, b)
	}
	slices.Reverse(history)

	sdb, err := state.New(lastSettled.execution.stateRootPost, state.NewDatabase(db), nil)
	if err != nil {
		return nil, 0, err
	}
	signer := vm.signer()
	checker := validityChecker{
		db:       sdb,
		gasClock: lastSettled.execution.by.clone(),
		nonces:   make(map[common.Address]uint64),
		balances: make(map[common.Address]*uint256.Int),
	}

	// TODO(arr4n): investigate caching values to avoid having to replay the
	// entire history.
	for _, b := range history {
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
		}
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
	settleAt := boundedSubtract(timestamp, stateRootDelaySeconds, vm.genesisTimestamp)

	for ; ; block, child = block.parent, block {
		if block.parent == nil {
			// The genesis block, by definition, is the lowest-height block to
			// be "settled".
			return block, true
		}

		if block.executed.Load() {
			if block.execution.by.time > settleAt {
				continue
			}
		} else if block.Time() <= settleAt {
			// TODO(arr4n) loosen this criterion, possibly introspecting the
			// current state of execution to check if the block can't execute in
			// time.
			return nil, false
		} else {
			continue
		}

		if block == parent {
			// Since the next block would be the one currently being built,
			// which we know to be at least [stateRootDelaySeconds] later,
			// `parent` would be the last one to execute in time for settlement.
			return block, true
		}

		if child.executed.Load() { // implies settled at `t > settleAt`
			// `block` is already known to be the last one to execute in time
			// for settlement.
			return block, true
		}

		if child.mightFitWithinGasLimit(block.execution.by.remainingGasThisSecond()) {
			// If we were to call this function again after `child` finishes
			// executing, `child` might be the return value. The result is
			// therefore uncertain.
			return nil, false
		}
		return block, true
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
