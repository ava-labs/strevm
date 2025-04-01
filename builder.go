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
	accepted sink.Mutex[*txTranche]
	snaps    sink.Monitor[*snapshot.Tree] // chunk-specific account inspection

	log logging.Logger

	// Development double (like a test double, but with alliteration)
	mempool chan *types.Transaction
	rng     *rand.Rand // reproducible blocks
}

type txTranche struct {
	prev     *txTranche // nil i.f.f. this is the root tranche, which MUST be `accepted`
	accepted bool       // if true then all transitive `prev` MUST also be true

	pending queue.FIFO[*pendingTx]
	// Worst-case bounds at the end of the `pending` queue. If the queue is
	// empty then these MUST match actual state; i.e. `excess` is the same value
	// as at the end of the last chunk and `deficits` are all zero.
	excess   gas.Gas
	deficits map[common.Address]*uint256.Int // relative to snapshot

	proposed []*types.Transaction // matches `pending`
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

func (tr *txTranche) withHistory(fn func(*txTranche)) {
	for ; tr != nil; tr = tr.prev {
		fn(tr)
	}
}

func (tr *txTranche) cumulativeExcess() gas.Gas {
	var sum gas.Gas
	tr.withHistory(func(tr *txTranche) {
		sum += tr.excess
	})
	return sum
}

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
	tr.withHistory(func(tr *txTranche) {
		if d := tr.deficits[addr]; d != nil {
			sum.Add(sum, d)
		}
	})
	return sum
}

func (bb *blockBuilder) build(
	ctx context.Context,
	parent *types.Block,
	chainConfig *params.ChainConfig,
	gasConfig *gas.Config,
	chunk *chunk,
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

	root := chunk.stateRootPost
	signer := types.LatestSigner(chainConfig)
	tranche, err := bb.makeTranche(ctx, nil /*last accepted tranche*/, root, signer, pool, gasConfig)
	if err != nil {
		return nil, nil, err
	}

	return tranche, types.NewBlockWithHeader(&types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).SetUint64(parent.NumberU64() + 1),
		Time:       chunk.timestamp + stateRootDelaySeconds,
		Root:       root,
	}).WithBody(types.Body{
		Transactions: tranche.proposed,
	}), nil
}

func (bb *blockBuilder) makeTranche(ctx context.Context, prevTranche *txTranche, stateRoot common.Hash, signer types.Signer, txs types.Transactions, cfg *gas.Config) (*txTranche, error) {
	return sink.FromMonitor(ctx, bb.snaps,
		func(t *snapshot.Tree) bool {
			return t.Snapshot(stateRoot) != nil
		},
		func(t *snapshot.Tree) (*txTranche, error) {
			snap := t.Snapshot(stateRoot)
			return sink.FromMutex(ctx, bb.accepted, func(accepted *txTranche) (*txTranche, error) {
				if prevTranche == nil {
					prevTranche = accepted
				}
				tranche := newTxTranche(prevTranche)

				for _, tx := range txs {
					price := tranche.gasPrice(cfg)
					gasLim := gas.Gas(tx.Gas())

					fee := uint256.NewInt(uint64(price))
					fee.Mul(fee, uint256.NewInt(uint64(gasLim)))
					cost := new(uint256.Int).Add(fee, uint256.MustFromBig(tx.Value()))

					from, err := types.Sender(signer, tx)
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
					tranche.proposed = append(tranche.proposed, tx)
				}
				return tranche, nil
			})
		})
}

func (bb *blockBuilder) acceptTranche(ctx context.Context, tr *txTranche) error {
	return bb.accepted.Use(ctx, func(root *txTranche) error {
		if !tr.prev.accepted {
			return fmt.Errorf("cannot accept %T before its predecessor", tr)
		}
		for tr.pending.Len() > 0 {
			// TODO(arr4n) implement concatenation on [queue.FIFO]
			root.pending.Push(tr.pending.Pop())
		}
		root.excess += tr.excess
		for addr, def := range tr.deficits {
			root.spend(addr, def)
		}
		tr.deficits = make(map[common.Address]*uint256.Int)
		tr.accepted = true
		return nil
	})
}

func (bb *blockBuilder) clearExecuted(ctx context.Context, chunk *chunk) error {
	return bb.accepted.Use(ctx, func(tranche *txTranche) error {
		if tranche.prev != nil {
			return fmt.Errorf("*BUG* only the root %T can be cleared", tranche)
		}

		for _, r := range chunk.receipts {
			if tranche.pending.Len() == 0 {
				return fmt.Errorf("*BUG* empty pending-tx queue when clearing receipt for %#x", r.TxHash)
			}
			if tx := tranche.pending.Peek().tx; tx.Hash() != r.TxHash {
				return fmt.Errorf("*BUG* receipt for tx %#x when next pending is %#x", r.TxHash, tx.Hash())
			}

			clear := tranche.pending.Pop()
			def := tranche.deficits[clear.from]
			if def.Cmp(clear.costUpperBound) == -1 {
				return fmt.Errorf("*BUG* for account %#x, deficit %s < pending cost to be cleared %s", clear.from, def.String(), clear.costUpperBound.String())
			}
			def.Sub(def, clear.costUpperBound)
			if def.IsZero() {
				delete(tranche.deficits, clear.from)
			}

			tranche.excess -= gas.Gas(clear.tx.Gas()>>1 - r.GasUsed>>1)
		}

		reduce := chunk.excessReduction
		if tranche.excess < reduce {
			return fmt.Errorf(
				"*BUG* chunk at %d reduced gas excess (%s) by more that block builder's worst-case prediction (%s)",
				chunk.timestamp, human(reduce), human(tranche.excess),
			)
		}
		tranche.excess -= reduce

		emptyQueue := tranche.pending.Len() == 0
		emptyDeficits := len(tranche.deficits) == 0
		if emptyQueue != emptyDeficits {
			return fmt.Errorf(
				"*BUG* block-builder deficits must be empty (%t) i.f.f. pending queue is empty (%t)",
				emptyDeficits, emptyQueue,
			)
		}
		excessMatch := tranche.excess == chunk.excessPost
		if emptyQueue != excessMatch {
			return fmt.Errorf(
				"*BUG* block builder's worst-case gas excess must match end of chunk (%t) i.f.f. pending queue is empty (%t)",
				excessMatch, emptyQueue,
			)
		}
		return nil
	})
}
