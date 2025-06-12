package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/worstcase"
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
		zap.Int("transactions", len(block.Transactions())),
	)
	return block, nil
}

var (
	errWaitingForExecution = errors.New("waiting for execution when building block")
	errNoopBlock           = errors.New("block does not settle state nor include transactions")
)

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
	// We can never concurrently build and accept a block on the same parent,
	// which guarantees that `parent` won't be settled, so the [Block] invariant
	// means that `parent.lastSettled != nil`.
	for _, b := range parent.IfChildSettles(toSettle) {
		brs := b.Receipts()
		receipts = append(receipts, brs)
		for _, r := range brs {
			gasUsed += r.GasUsed
		}
	}

	txs, gasLimit, err := vm.buildBlockOnHistory(toSettle, parent, timestamp, candidateTxs)
	if err != nil {
		return nil, err
	}
	if gasUsed == 0 && len(txs) == 0 {
		return nil, errNoopBlock
	}

	ethB := types.NewBlock(
		&types.Header{
			ParentHash: parent.Hash(),
			Root:       toSettle.PostExecutionStateRoot(),
			Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
			GasLimit:   uint64(gasLimit),
			GasUsed:    gasUsed,
			Time:       timestamp,
			BaseFee:    nil, // TODO(arr4n)
		},
		txs, nil, /*uncles*/
		slices.Concat(receipts...),
		trieHasher(),
	)
	return blocks.New(ethB, parent, toSettle, vm.logger())
}

func (vm *VM) buildBlockOnHistory(lastSettled, parent *Block, timestamp uint64, candidateTxs queue.Queue[*pendingTx]) (_ types.Transactions, _ gas.Gas, retErr error) {
	var history []*Block
	for b := parent; b.ID() != lastSettled.ID(); b = b.ParentBlock() {
		history = append(history, b)
	}
	slices.Reverse(history)

	sdb, err := state.New(lastSettled.PostExecutionStateRoot(), vm.exec.StateCache(), nil)
	if err != nil {
		return nil, 0, err
	}

	checker := worstcase.NewTxIncluder(
		sdb, vm.exec.ChainConfig(),
		lastSettled.ExecutedByGasTime().Clone(),
		5, 2, // TODO(arr4n) what are the max queue and block seconds?
	)

	for _, b := range history {
		checker.StartBlock(b.Header(), vm.hooks.GasTarget(b.ParentBlock().Block))
		for _, tx := range b.Transactions() {
			if err := checker.Include(tx); err != nil {
				vm.logger().Error(
					"Transaction not included when replaying history",
					zap.Stringer("block", b.Hash()),
					zap.Stringer("tx", tx.Hash()),
					zap.Error(err),
				)
				return nil, 0, err
			}
		}
	}

	var (
		include []*pendingTx
		delayed []*pendingTx
	)
	defer func() {
		for _, tx := range delayed {
			candidateTxs.Push(tx)
		}
		if retErr != nil {
			for _, tx := range include {
				candidateTxs.Push(tx)
			}
		}
	}()

	hdr := &types.Header{
		Number: new(big.Int).SetUint64(parent.NumberU64() + 1),
		Time:   timestamp,
	}
	checker.StartBlock(hdr, vm.hooks.GasTarget(parent.Block))

	for full := false; !full && candidateTxs.Len() > 0; {
		candidate := candidateTxs.Pop()
		tx := candidate.tx

		switch err := checker.Include(tx); {
		case err == nil:
			include = append(include, candidate)

		case errIsOneOf(err, worstcase.ErrBlockTooFull, worstcase.ErrQueueTooFull):
			delayed = append(delayed, candidate)
			full = true

		// TODO(arr4n) handle all other errors.

		default:
			vm.logger().Error(
				"Unknown error from worst-case transaction checking",
				zap.Error(err),
			)
			return nil, 0, err
		}
	}

	// TODO(arr4n) setting `gasUsed` (i.e. historical) based on receipts and
	// `gasLimit` (future-looking) based on the enqueued transactions follows
	// naturally from all of the other changes. However it will be possible to
	// have `gasUsed>gasLimit`, which may break some systems.
	var gasLimit gas.Gas
	txs := make(types.Transactions, len(include))
	for i, tx := range include {
		txs[i] = tx.tx
		gasLimit += gas.Gas(tx.tx.Gas())
	}

	// TODO(arr4n) return the base fee too, available from the [gastime.Time] in
	// `checker`.

	return txs, gasLimit, nil
}

func errIsOneOf(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

func (vm *VM) lastBlockToSettleAt(timestamp uint64, parent *Block) (*Block, bool) {
	settleAt := intmath.BoundedSubtract(timestamp, stateRootDelaySeconds, vm.last.synchronousTime)
	return blocks.LastToSettleAt(settleAt, parent)
}
