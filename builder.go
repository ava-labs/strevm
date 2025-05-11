package sae

import (
	"context"
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
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/queue"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

type blockBuilder struct {
	tranches sink.Monitor[*tranches]
	snaps    sink.Monitor[*snapshot.Tree] // chunk-specific account inspection

	log logging.Logger

	mempool sink.PriorityMutex[*mempool]
	newTxs  chan *transaction
	quit    <-chan struct{}
	done    chan<- struct{}
}

type tranches struct {
	// Each [txTranche] has a `prev` pointer that is used to form a tree
	// (mirroring consensus) with this `accepted` tranche as the root. When a
	// block is accepted, its tranche is collapsed into the `accepted` one (but
	// not removed from the block, just emptied).
	//
	// From the perspective of a single tranche, there is a linked list of its
	// ancestors, and methods like [txTranche.cumulativeExcess] traverse the
	// list.
	//
	// When executed chunks are cleared (from the `accepted` tranche's `pending`
	// queue), we store a "pre-historical" tranche per chunk. In block building
	// and verification, the `cumulative*` methods can therefore be made to
	// terminate at any point in time by setting the `prev` pointers
	// appropriately, terminating the methods when `prev==nil`. It is safe to do
	// this as these are protected by a mutex, but `accepted` MUST be returned
	// with a nil `prev`.
	accepted      *txTranche
	chunkTranches map[uint64]*txTranche
}

type txTranche struct {
	prev     *txTranche // nil i.f.f. this is the root tranche, which MUST be `accepted`
	accepted bool       // if true then all transitive `prev` MUST also be true

	pending queue.FIFO[*pendingTx]
	// The time *before* the queue is only relevant for extending into
	// "pre-historic" tranches, allowing the `accepted`` tranche to select the
	// appropriate `prev`.
	lastExecutedTimeBeforeQueue uint64
	// Worst-case bounds at the *end* of the `pending` queue. If the queue is
	// empty then these MUST match actual state; i.e. `excess` is the same value
	// as at the end of the last chunk and `deficits` are all zero.
	excess   gas.Gas
	deficits map[common.Address]*uint256.Int // relative to snapshot

	rawTxs []*types.Transaction // matches `pending`
	// TODO(arr4n) add fields for skipped (temporary) and rejected (permanent)
	// transactions.
}

type pendingTx struct {
	tx             *types.Transaction
	from           common.Address
	costUpperBound *uint256.Int
}

func newRootTxTranche() *txTranche {
	// By definition, the root has no parent and is accepted.
	t := newTxTranche(nil /*parent*/)
	t.accepted = true
	return t
}

func newTxTranche(prev *txTranche) *txTranche {
	return &txTranche{
		prev:     prev,
		deficits: make(map[common.Address]*uint256.Int),
	}
}

// gasPrice returns the worst-case gas price at the end of the tranche's queue.
func (tr *txTranche) gasPrice(cfg *gas.Config) gas.Price {
	return gas.CalculatePrice(cfg.MinPrice, tr.cumulativeExcess(), cfg.ExcessConversionConstant)
}

func (tr *txTranche) callOnAncestry(fn func(*txTranche)) {
	for ; tr != nil; tr = tr.prev {
		fn(tr)
	}
}

func (tr *txTranche) cumulativeExcess() gas.Gas {
	var sum gas.Gas
	tr.callOnAncestry(func(tr *txTranche) {
		sum += tr.excess
	})
	return sum
}

// spend adds to the deficit for the account; use [txTranche.cumulativeDeficit]
// to recover the sum of all calls to spend() across the entire tranche history.
func (tr *txTranche) spend(addr common.Address, amt *uint256.Int) {
	d, ok := tr.deficits[addr]
	if !ok {
		d = new(uint256.Int)
		tr.deficits[addr] = d
	}
	d.Add(d, amt)
}

func (tr *txTranche) cumulativeDeficit(addr common.Address) *uint256.Int {
	sum := new(uint256.Int)
	tr.callOnAncestry(func(tr *txTranche) {
		if d := tr.deficits[addr]; d != nil {
			sum.Add(sum, d)
		}
	})
	return sum
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
	return block, nil
}

func (bb *blockBuilder) buildBlockWithCandidateTxs(timestamp uint64, parent *Block, candidateTxs queue.Queue[*transaction]) (*Block, error) {
	toSettle, ok := lastBlockToSettleAt(timestamp, parent)
	if !ok {
		bb.log.Warn(
			"Block building waiting for execution",
			zap.Uint64("timestamp", timestamp),
			zap.Stringer("parent", parent.ID()),
		)
		return nil, fmt.Errorf("waiting for execution when building block on %#x at time %d", parent.ID(), timestamp)
	}

	var (
		receipts []types.Receipts
		gasUsed  uint64
	)
	for b := toSettle; b.ID() != parent.lastSettled.ID(); b = b.parent {
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
	var txs types.Transactions // TODO(arr4n) populate from `candidateTxs`
	var gasLimit uint64
	for _, tx := range txs {
		gasLimit += tx.Gas()
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
	}, fmt.Errorf("unimplemented")
}

func lastBlockToSettleAt(timestamp uint64, parent *Block) (*Block, bool) {
	if parent.parent == nil {
		// The genesis block, by definition, is the lowest-height block to
		// be "settled".
		return parent, true
	}

	// These variables are only abstracted for clarity; they are not needed
	// beyond the scope of the `for` loop.
	var block, child *Block
	block = parent // therefore `child` remains nil
	settleAt := timestamp - stateRootDelaySeconds

	for ; ; block, child = block.parent, block {
		if !block.executed.Load() || block.execution.by.time > settleAt {
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

func (bb *blockBuilder) build(
	ctx context.Context,
	parent *types.Block,
	chunk *chunk,
	chainConfig *params.ChainConfig,
	gasConfig *gas.Config,
) (*txTranche, *types.Block, error) {
	// The only other use of the mempool is the background worker that adds txs,
	// so we ignore any attempts at preemption from it. Although that worker
	// ignores our priority and *always* returns early, we defensively set it as
	// the highest possible to protect against future refactors.
	var tranche *txTranche
	err := bb.mempool.Use(ctx, sink.Priority(math.MaxUint), func(_ <-chan sink.Priority, mp *mempool) error {
		// TODO(arr4n) implement sink.FromPriorityMutex()
		cfg := &trancheBuilderConfig{
			atEndOf:    chunk,
			candidates: &mp.pool,
			gasConfig:  gasConfig,
		}
		var err error
		tranche, err = bb.makeTranche(ctx, cfg)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	return tranche, types.NewBlockWithHeader(&types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).SetUint64(parent.NumberU64() + 1),
		Time:       chunk.timestamp + stateRootDelaySeconds,
		Root:       chunk.stateRootPost,
	}).WithBody(types.Body{
		Transactions: tranche.rawTxs,
	}), nil
}

type trancheBuilderConfig struct {
	// prev allows tranches to be built on top of preferred blocks that aren't
	// accepted yet. If nil, the tranche will be built on top of
	// [tranches.accepted] once unlocked from its mutex.
	prev       *txTranche
	atEndOf    *chunk
	candidates queue.Queue[*transaction]
	gasConfig  *gas.Config
}

func (bb *blockBuilder) makeTranche(ctx context.Context, cfg *trancheBuilderConfig) (*txTranche, error) {
	return sink.FromMonitors(ctx, bb.tranches, bb.snaps,
		func(trs *tranches) bool {
			_, ok := trs.chunkTranches[cfg.atEndOf.timestamp]
			return ok || cfg.atEndOf.isGenesis()
		},
		func(t *snapshot.Tree) bool {
			return t.Snapshot(cfg.atEndOf.stateRootPost) != nil
		},
		func(trs *tranches, snaps *snapshot.Tree) (*txTranche, error) {
			defer func() {
				trs.accepted.prev = nil
			}()
			for sentinel := trs.accepted; sentinel.lastExecutedTimeBeforeQueue > cfg.atEndOf.timestamp; {
				at := sentinel.lastExecutedTimeBeforeQueue - 1
				before, ok := trs.chunkTranches[at]
				// TODO(arr4n) at the moment we only build blocks exactly 1
				// second after the parent so this loop will never be entered.
				// Additionally, the condition on the tranches [sink.Monitor]
				// should be set correctly to avoid this. This check is
				// therefore a defensive mechanism to address when allowing for
				// blocks with a greater gap.
				if !ok {
					return nil, fmt.Errorf("*BUG* chunk tranche at time %d missing while connecting tranche history", at)
				}
				sentinel.prev = before
			}

			prev := cfg.prev
			if prev == nil {
				prev = trs.accepted
			}
			tranche := newTxTranche(prev)

			snap := snaps.Snapshot(cfg.atEndOf.stateRootPost)

			// TODO(arr4n) there's a tradeoff between clearing the mempool and
			// starting execution, hence the currently arbitrary cap. This needs
			// to be explored more deeply. It also needs to be removed entirely
			// for block verification!
			for i := 0; i < 200 && cfg.candidates.Len() != 0; i++ {
				poolTx := cfg.candidates.Pop()
				tx := poolTx.tx
				// TODO(arr4n) stop returning errors and, instead:
				// (a) set poolTx.timePriority = time.Now(); and
				// (b) increment poolTx.retryAttempts, discarding if too high.

				price := tranche.gasPrice(cfg.gasConfig)
				gasLim := gas.Gas(tx.Gas())

				fee := uint256.NewInt(uint64(price))
				fee.Mul(fee, uint256.NewInt(uint64(gasLim)))
				cost := new(uint256.Int).Add(fee, uint256.MustFromBig(tx.Value()))

				from := poolTx.from
				fromHash := crypto.Keccak256Hash(from[:])
				account, err := snap.Account(fromHash)
				if err != nil {
					return nil, err
				}
				if account == nil {
					return nil, fmt.Errorf("empty origin account %v can't cover cost of tx %#x", from, tx.Hash())
				}

				if bal := new(uint256.Int).Sub(account.Balance, tranche.cumulativeDeficit(from)); bal.Cmp(cost) == -1 {
					return nil, fmt.Errorf("account %v has insufficient balance (%v) to cover worst-case cost (%v) of tx %#x", from, bal, cost, tx.Hash())
				}
				tranche.spend(from, cost)

				tranche.excess += gasLim >> 1
				tranche.pending.Push(&pendingTx{
					tx:             tx,
					from:           from,
					costUpperBound: cost,
				})
				tranche.rawTxs = append(tranche.rawTxs, tx)
			}
			return tranche, nil
		},
	)
}

func (bb *blockBuilder) acceptTranche(ctx context.Context, tr *txTranche) error {
	return bb.tranches.UseThenSignal(ctx, func(trs *tranches) error {
		// This check needs to be performed inside the lock because tranches
		// have their `accepted` flag set a few lines down.
		if !tr.prev.accepted {
			return fmt.Errorf("cannot accept %T before its predecessor", tr)
		}

		base := trs.accepted
		// There MUST be matching but inverse modifications to `base` and `tr`
		// to result in a "relocation" of pending txs.
		for tr.pending.Len() > 0 {
			// TODO(arr4n) implement concatenation on [queue.FIFO]
			base.pending.Push(tr.pending.Pop())
		}

		base.excess += tr.excess
		tr.excess = 0

		for addr, def := range tr.deficits {
			base.spend(addr, def)
		}
		tr.deficits = make(map[common.Address]*uint256.Int)

		tr.accepted = true
		return nil
	})
}

func (bb *blockBuilder) clearExecuted(ctx context.Context, chunk *chunk) error {
	return bb.tranches.UseThenSignal(ctx, func(trs *tranches) error {
		accepted := trs.accepted

		prev, ok := trs.chunkTranches[chunk.timestamp-1]
		if !ok && !chunk.isGenesis() { // genesis has no prev
			return fmt.Errorf("clearing %T @ time %d before its predecessor", chunk, chunk.timestamp)
		}
		chunkTranche := newTxTranche(prev)
		// TODO(arr4n) GC the chunk tranches, probably after [stateRootDelaySeconds]

		for _, r := range chunk.receipts {
			if accepted.pending.Len() == 0 {
				return fmt.Errorf("*BUG* empty pending-tx queue when clearing receipt for %#x", r.TxHash)
			}
			if tx := accepted.pending.Peek().tx; tx.Hash() != r.TxHash {
				return fmt.Errorf("*BUG* receipt for tx %#x when next pending is %#x", r.TxHash, tx.Hash())
			}

			// There MUST be matching but inverse modifications to `accepted`
			// and `chunkTranche` to result in a "relocation" of executed txs.
			clear := accepted.pending.Pop()
			chunkTranche.pending.Push(clear)

			def := accepted.deficits[clear.from]
			if def.Cmp(clear.costUpperBound) == -1 {
				return fmt.Errorf("*BUG* for account %#x, deficit %s < pending cost to be cleared %s", clear.from, def.String(), clear.costUpperBound.String())
			}
			def.Sub(def, clear.costUpperBound)
			chunkTranche.spend(clear.from, clear.costUpperBound)
			if def.IsZero() {
				delete(accepted.deficits, clear.from)
			}

			dExcess := gas.Gas(clear.tx.Gas()>>1 - r.GasUsed>>1)
			accepted.excess -= dExcess
			chunkTranche.excess += dExcess
		}

		reduce := chunk.excessReduction
		if accepted.excess < reduce {
			return fmt.Errorf(
				"*BUG* chunk at %d reduced gas excess (%s) by more that block builder's worst-case prediction (%s)",
				chunk.timestamp, human(reduce), human(accepted.excess),
			)
		}
		accepted.excess -= reduce
		chunkTranche.excess += reduce

		accepted.lastExecutedTimeBeforeQueue = chunk.timestamp
		chunkTranche.lastExecutedTimeBeforeQueue = chunk.timestamp - 1

		trs.chunkTranches[chunk.timestamp] = chunkTranche

		emptyQueue := accepted.pending.Len() == 0
		emptyDeficits := len(accepted.deficits) == 0
		if emptyQueue != emptyDeficits {
			return fmt.Errorf(
				"*BUG* block-builder deficits must be empty (%t) i.f.f. pending queue is empty (%t)",
				emptyDeficits, emptyQueue,
			)
		}
		excessMatch := accepted.excess == chunk.excessPost
		if emptyQueue != excessMatch {
			return fmt.Errorf(
				"*BUG* block builder's worst-case gas excess must match end of chunk (%t) i.f.f. pending queue is empty (%t)",
				excessMatch, emptyQueue,
			)
		}
		return nil
	})
}
