package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/queue"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

type blockBuilder struct {
	genesisTimestamp uint64
	snaps            sink.Monitor[*snapshot.Tree] // block-specific account inspection

	log logging.Logger

	mempool sink.PriorityMutex[*mempool]
	newTxs  chan *transaction
	quit    <-chan struct{}
	done    chan<- struct{}
}

type pendingTx struct {
	tx             *types.Transaction
	from           common.Address
	costUpperBound *uint256.Int
}

func (bb *blockBuilder) buildBlock(ctx context.Context, timestamp uint64, parent *Block) (*Block, error) {
	// TODO(arr4n) implement sink.FromPriorityMutex()
	var block *Block
	if err := bb.mempool.Use(ctx, sink.Priority(math.MaxUint64), func(preempt <-chan sink.Priority, mp *mempool) error {
		var err error
		block, err = bb.buildBlockWithCandidateTxs(timestamp, parent, &mp.pool)
		return err
	}); err != nil {
		return nil, err
	}
	bb.log.Debug(
		"Built block",
		zap.Uint64("timestamp", timestamp),
		zap.Uint64("height", block.Height()),
		zap.Stringer("parent", parent.Hash()),
	)
	return block, nil
}

var errWaitingForExecution = errors.New("waiting for execution when building block")

func (bb *blockBuilder) buildBlockWithCandidateTxs(timestamp uint64, parent *Block, candidateTxs queue.Queue[*transaction]) (*Block, error) {
	toSettle, ok := bb.lastBlockToSettleAt(timestamp, parent)
	if !ok {
		bb.log.Warn(
			"Block building waiting for execution",
			zap.Uint64("timestamp", timestamp),
			zap.Stringer("parent", parent.Hash()),
		)
		return nil, fmt.Errorf("%w: parent %#x at time %d", errWaitingForExecution, parent.Hash(), timestamp)
	}
	bb.log.Debug(
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

	// TODO(arr4n) setting `gasUsed` (i.e. historical) based on receipts and
	// `gasLimit` (future-looking) based on the enqueued transactions follows
	// naturally from all of the other changes. However it will be possible to
	// have `gasUsed>gasLimit`, which may break some systems.
	var gasLimit uint64

	var txs types.Transactions
	for i := 0; i < 200 && candidateTxs.Len() > 0; i++ {
		// TODO(arr4n) worst-case validity checks go here.
		tx := candidateTxs.Pop().tx
		gasLimit += tx.Gas()
		txs = append(txs, tx)
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

func (bb *blockBuilder) lastBlockToSettleAt(timestamp uint64, parent *Block) (*Block, bool) {
	// These variables are only abstracted for clarity; they are not needed
	// beyond the scope of the `for` loop.
	var block, child *Block
	block = parent // therefore `child` remains nil
	settleAt := boundedSubtract(timestamp, stateRootDelaySeconds, bb.genesisTimestamp)

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
