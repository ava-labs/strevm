package sae

import (
	"context"
	"fmt"
	"math/big"
	"math/rand/v2"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/queue"
	"github.com/holiman/uint256"
)

type blockBuilder struct {
	tranches sink.Monitor[*tranches]
	snaps    sink.Monitor[*snapshot.Tree] // chunk-specific account inspection

	log logging.Logger

	// Development double (like a test double, but with alliteration)
	mempool chan *types.Transaction
	rng     *rand.Rand // reproducible blocks
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

func (bb *blockBuilder) build(
	ctx context.Context,
	parent *types.Block,
	chunk *chunk,
	chainConfig *params.ChainConfig,
	gasConfig *gas.Config,
) (*txTranche, *types.Block, error) {

	max := bb.rng.IntN(5000)
	pool := make(types.Transactions, 0, max)
BuildLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case tx := <-bb.mempool:
			pool = append(pool, tx)
			if len(pool) == max {
				break BuildLoop
			}
		default:
			break BuildLoop
		}
	}

	cfg := &trancheBuilderConfig{
		atEndOf:    chunk,
		signer:     types.LatestSigner(chainConfig),
		candidates: pool,
		gasConfig:  gasConfig,
	}
	tranche, err := bb.makeTranche(ctx, cfg)
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
	signer     types.Signer
	candidates []*types.Transaction
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

			for _, tx := range cfg.candidates {
				price := tranche.gasPrice(cfg.gasConfig)
				gasLim := gas.Gas(tx.Gas())

				fee := uint256.NewInt(uint64(price))
				fee.Mul(fee, uint256.NewInt(uint64(gasLim)))
				cost := new(uint256.Int).Add(fee, uint256.MustFromBig(tx.Value()))

				from, err := types.Sender(cfg.signer, tx)
				if err != nil {
					return nil, err
				}
				fromHash := crypto.Keccak256Hash(from[:])
				account, err := snap.Account(fromHash)
				if err != nil {
					return nil, err
				}

				if bal := new(uint256.Int).Sub(account.Balance, tranche.cumulativeDeficit(from)); bal.Cmp(cost) == -1 {
					// TODO(arr4n) this will need to change (probably to a
					// `continue`) when this loop is a filter instead of a
					// validator of txs.
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
