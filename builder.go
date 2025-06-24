package sae

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/big"
	"slices"

	"github.com/arr4n/sink"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/worstcase"
	"go.uber.org/zap"
)

func (vm *VM) buildBlock(ctx context.Context, blockContext *block.Context, timestamp uint64, parent *blocks.Block) (*blocks.Block, error) {
	block, err := sink.FromPriorityMutex(
		ctx, vm.mempool, sink.MaxPriority,
		func(_ <-chan sink.Priority, pool *queue.Priority[*pendingTx]) (*blocks.Block, error) {
			block, err := vm.buildBlockWithCandidateTxs(timestamp, parent, pool, blockContext, vm.hooks.ConstructBlock)

			// TODO: This shouldn't be done immediately, there should be some
			// retry delay if block building failed.
			if pool.Len() > 0 {
				select {
				case vm.toEngine <- snowcommon.PendingTxs:
				default:
					p := snowcommon.PendingTxs
					vm.logger().Info(fmt.Sprintf("%T(%s) dropped", p, p))
				}
			}

			return block, err
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

func (vm *VM) buildBlockWithCandidateTxs(
	timestamp uint64,
	parent *blocks.Block,
	candidateTxs queue.Queue[*pendingTx],
	blockContext *block.Context,
	constructBlock hook.ConstructBlock,
) (*blocks.Block, error) {
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

	ethB, err := vm.buildBlockOnHistory(
		toSettle,
		parent,
		timestamp,
		candidateTxs,
		blockContext,
		constructBlock,
	)
	if err != nil {
		return nil, err
	}

	// TODO: Check if the block contains transactions (potentially atomic)
	if ethB.GasUsed() == 0 {
		vm.logger().Info("Blocks must either settle or include transactions")
		return nil, fmt.Errorf("%w: parent %#x at time %d", errNoopBlock, parent.Hash(), timestamp)
	}

	return blocks.New(ethB, parent, toSettle, vm.logger())
}

func (vm *VM) buildBlockOnHistory(
	lastSettled,
	parent *blocks.Block,
	timestamp uint64,
	candidateTxs queue.Queue[*pendingTx],
	blockContext *block.Context,
	constructBlock hook.ConstructBlock,
) (_ *types.Block, retErr error) {
	var history []*blocks.Block
	for b := parent; b.ID() != lastSettled.ID(); b = b.ParentBlock() {
		history = append(history, b)
	}
	slices.Reverse(history)

	sdb, err := state.New(lastSettled.PostExecutionStateRoot(), vm.exec.StateCache(), nil)
	if err != nil {
		return nil, err
	}

	checker := worstcase.NewTxIncluder(
		sdb, vm.exec.ChainConfig(),
		lastSettled.ExecutedByGasTime().Clone(),
		5, 2, // TODO(arr4n) what are the max queue and block seconds?
	)

	for _, b := range history {
		checker.StartBlock(b.Header(), vm.hooks.GasTarget(b.ParentBlock().Block))
		for _, tx := range b.Transactions() {
			if err := checker.ApplyTx(tx); err != nil {
				vm.logger().Error(
					"Transaction not included when replaying history",
					zap.Stringer("block", b.Hash()),
					zap.Stringer("tx", tx.Hash()),
					zap.Error(err),
				)
				return nil, err
			}
		}

		extraOps, err := vm.hooks.ExtraBlockOperations(context.TODO(), b.Block)
		if err != nil {
			vm.logger().Error(
				"Unable to extract extra block operations when replaying history",
				zap.Stringer("block", b.Hash()),
				zap.Error(err),
			)
			return nil, err
		}
		for i, op := range extraOps {
			if err := checker.Apply(op); err != nil {
				vm.logger().Error(
					"Operation not applied when replaying history",
					zap.Stringer("block", b.Hash()),
					zap.Int("index", i),
					zap.Error(err),
				)
				return nil, err
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

		switch err := checker.ApplyTx(tx); {
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

			// TODO: It is not acceptable to return an error here, as all
			// transactions that have been removed from the mempool will be
			// dropped and never included.
			return nil, err
		}
	}

	var (
		receipts []types.Receipts
		gasUsed  uint64
	)
	// We can never concurrently build and accept a block on the same parent,
	// which guarantees that `parent` won't be settled, so the [Block] invariant
	// means that `parent.lastSettled != nil`.
	for _, b := range parent.IfChildSettles(lastSettled) {
		brs := b.Receipts()
		receipts = append(receipts, brs)
		for _, r := range brs {
			gasUsed += r.GasUsed
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

	header := &types.Header{
		ParentHash: parent.Hash(),
		Root:       lastSettled.PostExecutionStateRoot(),
		Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
		GasLimit:   uint64(gasLimit),
		GasUsed:    gasUsed,
		Time:       timestamp,
		BaseFee:    nil, // TODO(arr4n)
	}
	ancestors := iterateUntilSettled(parent)
	return constructBlock(
		context.TODO(),
		blockContext,
		header,
		parent.Block.Header(),
		ancestors,
		checker,
		txs,
		slices.Concat(receipts...),
	)
}

func errIsOneOf(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

func (vm *VM) lastBlockToSettleAt(timestamp uint64, parent *blocks.Block) (*blocks.Block, bool) {
	settleAt := intmath.BoundedSubtract(timestamp, stateRootDelaySeconds, vm.last.synchronous.time)
	return blocks.LastToSettleAt(settleAt, parent)
}

// iterateUntilSettled returns an iterator which starts at the provided block
// and iterates up to but not including the most recently settled block.
//
// If the provided block is settled, then the returned iterator is empty.
func iterateUntilSettled(from *blocks.Block) iter.Seq[*types.Block] {
	return func(yield func(*types.Block) bool) {
		// Do not modify the `from` variable to support multiple iterations.
		current := from
		for {
			next := current.ParentBlock()
			// If the next block is nil, then the current block is settled.
			if next == nil {
				return
			}

			// If the person iterating over this iterator broke out of the loop,
			// we must not call yield again.
			if !yield(current.Block) {
				return
			}

			current = next
		}
	}
}
