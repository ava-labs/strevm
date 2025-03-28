package sae

import (
	"context"
	"fmt"
	"math/big"
	"time"

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
	pending queue.FIFO[*pendingInclusion]
	// Worst-case bounds at the end of the `pending` queue. If the queue is
	// empty then these MUST match the actual state of the [executor]; i.e.
	// `excess` is the same value and `deficits` are all zero.
	excess   gas.Gas
	deficits map[common.Address]*uint256.Int
	already  gas.Gas
	snaps    sink.Monitor[*snapshot.Tree]

	log logging.Logger

	// Development double (like a test double, but with alliteration)
	mempool chan *types.Transaction
}

func (bb *blockBuilder) gasPrice(cfg *gas.Config) gas.Price {
	return gas.CalculatePrice(cfg.MinPrice, bb.excess, cfg.ExcessConversionConstant)
}

func (bb *blockBuilder) build(
	ctx context.Context,
	parent *types.Block,
	chainConfig *params.ChainConfig,
	gasConfig *gas.Config,
	chunk *chunk,
) (*types.Block, error) {
	txs := make(types.Transactions, 0, 100_000)
	stop := time.After(3 * time.Millisecond)

BuildLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case tx := <-bb.mempool:
			txs = append(txs, tx)
		case <-stop:
			break BuildLoop
		}
	}

	if err := bb.clearPending(chunk); err != nil {
		return nil, err
	}
	signer := types.LatestSigner(chainConfig)
	accepted, err := bb.addToPending(ctx, chunk.stateRootPost, signer, txs, gasConfig)
	if err != nil {
		return nil, err
	}

	return types.NewBlockWithHeader(&types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).SetUint64(parent.NumberU64() + 1),
		Time:       chunk.time + stateRootDelaySeconds,
		Root:       chunk.stateRootPost,
	}).WithBody(types.Body{
		Transactions: accepted,
	}), nil
}

type pendingInclusion struct {
	tx                  *types.Transaction
	from                common.Address
	costUpperBound      *uint256.Int
	predictedExcessPost gas.Gas
}

func (bb *blockBuilder) addToPending(ctx context.Context, stateRoot common.Hash, signer types.Signer, txs types.Transactions, cfg *gas.Config) (types.Transactions, error) {
	return sink.FromMonitor(ctx, bb.snaps,
		func(t *snapshot.Tree) bool {
			return t.Snapshot(stateRoot) != nil
		},
		func(t *snapshot.Tree) (types.Transactions, error) {
			snap := t.Snapshot(stateRoot)

			for _, tx := range txs {
				price := bb.gasPrice(cfg)
				gasLim := gas.Gas(tx.Gas())

				fee := uint256.NewInt(uint64(price))
				fee.Mul(fee, uint256.NewInt(uint64(gasLim)))
				cost := new(uint256.Int).Add(fee, uint256.MustFromBig(tx.Value()))

				from, err := types.Sender(signer, tx)
				if err != nil {
					return nil, err
				}

				deficit, ok := bb.deficits[from]
				if !ok {
					deficit = new(uint256.Int)
					bb.deficits[from] = deficit
				}

				fromHash := crypto.Keccak256Hash(from[:])
				acc, err := snap.Account(fromHash)
				if err != nil {
					return nil, err
				}
				if bal := new(uint256.Int).Sub(acc.Balance, deficit); bal.Cmp(cost) == -1 {
					// TODO(arr4n) this will need to change when this loop is a
					// filter instead of a validator of txs.
					return nil, fmt.Errorf("account %v has insufficient balance (%v) to cover worst-case cost (%v) of tx %#x", from, bal, cost, tx.Hash())
				}
				deficit.Add(deficit, cost)

				bb.excess += gasLim >> 1
				bb.pending.Push(&pendingInclusion{
					tx:                  tx,
					from:                from,
					costUpperBound:      cost,
					predictedExcessPost: bb.excess,
				})
			}
			return txs, nil
		})
}

func (bb *blockBuilder) clearPending(chunk *chunk) error {
	for _, r := range chunk.receipts {
		switch first, ok := bb.pending.Peek(); {
		case !ok:
			return fmt.Errorf("empty pending tx queue when clearing receipt for %#x", r.TxHash)
		case first.tx.Hash() != r.TxHash:
			return fmt.Errorf("receipt for tx %#x when next pending is %#x", r.TxHash, first.tx.Hash())
		}

		clear, _ := bb.pending.Pop() // dropped `ok` is guaranteed to be true because of the peek
		def := bb.deficits[clear.from]
		if def.Cmp(clear.costUpperBound) == -1 {
			return fmt.Errorf("for account %#x, deficit %s < pending cost to be cleared %s", clear.from, def.String(), clear.costUpperBound.String())
		}
		def.Sub(def, clear.costUpperBound)
		if def.IsZero() {
			delete(bb.deficits, clear.from)
		}

		bb.excess -= gas.Gas(clear.tx.Gas()>>1 - r.GasUsed>>1)
	}

	reduce := chunk.excessReduction
	if bb.excess < reduce {
		return fmt.Errorf("%s < %s", human(bb.excess), human(reduce))
	}
	bb.excess -= reduce

	emptyQueue := bb.pending.Len() == 0
	emptyDeficits := len(bb.deficits) == 0
	if emptyQueue != emptyDeficits {
		return fmt.Errorf("block-builder deficits must be empty (%t) i.f.f. pending queue is empty (%t)", emptyDeficits, emptyQueue)
	}
	excessMatch := bb.excess == chunk.excessPost
	if emptyQueue != excessMatch {
		return fmt.Errorf("block builder's worst-case gas excess must match end of chunk (%t) i.f.f. pending queue is empty (%t)", excessMatch, emptyQueue)
	}
	return nil
}
