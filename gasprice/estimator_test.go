// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/saetest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

type backend struct {
	lock   sync.RWMutex
	blocks []*blocks.Block // blocks[i] is block with number i

	headEvents event.FeedOf[core.ChainHeadEvent]
}

func newBackend(genesis *blocks.Block) *backend {
	return &backend{
		blocks: []*blocks.Block{genesis},
	}
}

func (b *backend) accept(blk *blocks.Block) {
	b.lock.Lock()
	b.blocks = append(b.blocks, blk)
	b.lock.Unlock()

	b.headEvents.Send(core.ChainHeadEvent{Block: blk.EthBlock()})
}

func (b *backend) ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := b.LastAcceptedBlock().NumberU64()
	switch bn {
	case rpc.EarliestBlockNumber:
		return 0, nil
	case rpc.FinalizedBlockNumber, rpc.SafeBlockNumber, rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		return head, nil
	default:
		if bn < 0 {
			return 0, fmt.Errorf("%s block unsupported", bn)
		}
		n := uint64(bn) //nolint:gosec // Non-negative checked above
		if n > head {
			return 0, fmt.Errorf("%w: block %d", errMissingBlock, n)
		}
		return n, nil
	}
}

func (b *backend) BlockByNumber(ctx context.Context, bn rpc.BlockNumber) (*types.Block, error) {
	n, err := b.ResolveBlockNumber(bn)
	if err != nil {
		return nil, err
	}

	b.lock.RLock()
	defer b.lock.RUnlock()

	return b.blocks[n].EthBlock(), nil
}

func (b *backend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.headEvents.Subscribe(ch)
}

func (b *backend) LastAcceptedBlock() *blocks.Block {
	b.lock.RLock()
	defer b.lock.RUnlock()

	return b.blocks[len(b.blocks)-1]
}

type SUT struct {
	*Estimator

	signer  types.Signer
	wallet  *saetest.Wallet
	builder *blockstest.ChainBuilder
	backend *backend
}

func newSUT(tb testing.TB, c Config) *SUT {
	tb.Helper()

	db := rawdb.NewMemoryDatabase()
	xdb := saetest.NewExecutionResultsDB()
	config := saetest.ChainConfig()
	signer := types.LatestSigner(config)
	wallet := saetest.NewUNSAFEWallet(tb, 1, signer)
	alloc := saetest.MaxAllocFor(wallet.Addresses()...)
	genesis := blockstest.NewGenesis(tb, db, xdb, config, alloc)
	builder := blockstest.NewChainBuilder(config, genesis)
	backend := newBackend(genesis)

	c.Log = saetest.NewTBLogger(tb, logging.Debug)
	c.Now = func() time.Time {
		return backend.LastAcceptedBlock().Timestamp()
	}
	e := NewEstimator(backend, c)
	tb.Cleanup(e.Close)

	return &SUT{
		Estimator: e,
		signer:    signer,
		wallet:    wallet,
		builder:   builder,
		backend:   backend,
	}
}

func (s *SUT) acceptBlock(b *blocks.Block) {
	s.backend.accept(b)
}

const gasLimit = 1_000_000

func (s *SUT) newBlock(tb testing.TB, time uint64, bounds *blocks.WorstCaseBounds, txs ...*types.Transaction) *blocks.Block {
	tb.Helper()
	blk := s.builder.NewBlock(tb, txs, blockstest.WithEthBlockOptions(
		blockstest.ModifyHeader(func(h *types.Header) {
			h.GasLimit = gasLimit
			h.GasUsed = 0
			for _, tx := range txs {
				h.GasUsed += tx.Gas()
			}
			h.Time = time
			h.BaseFee = h.Number
		}),
	))
	blk.SetWorstCaseBounds(bounds)
	return blk
}

func (s *SUT) newTx(tb testing.TB, gas, price uint64) *types.Transaction {
	tb.Helper()
	return s.wallet.SignTx(tb, s.signer, 0, &types.DynamicFeeTx{
		Gas:       gas,
		GasTipCap: new(big.Int).SetUint64(price),
		// Set the fee cap to a very large value so the tx tip is always the
		// tip cap.
		GasFeeCap: new(big.Int).SetUint64(math.MaxUint64),
	})
}

const (
	avax  = params.Ether
	nAVAX = params.GWei
	aAVAX = params.Wei
)

func TestSuggestTipCap(t *testing.T) {
	c := Config{
		MinSuggestedTip: big.NewInt(aAVAX),
		MaxSuggestedTip: big.NewInt(avax),
	}
	sut := newSUT(t, c)

	newBlock := func(time uint64, txs ...*types.Transaction) *blocks.Block {
		t.Helper()
		return sut.newBlock(t, time, nil, txs...)
	}
	newTx := func(price uint64) *types.Transaction {
		t.Helper()
		return sut.newTx(t, 1, price)
	}

	steps := []struct {
		name     string
		newBlock *blocks.Block
		want     *big.Int
	}{
		{
			name: "genesis",
			want: c.MinSuggestedTip,
		},
		{
			name:     "single_tx",
			newBlock: newBlock(100, newTx(nAVAX)),
			want:     big.NewInt(nAVAX),
		},
		{
			name:     "multiple_blocks",
			newBlock: newBlock(110, newTx(3*nAVAX), newTx(2*nAVAX)),
			want:     big.NewInt(nAVAX),
		},
		{
			name:     "increase_tip",
			newBlock: newBlock(110, newTx(4*nAVAX)),
			want:     big.NewInt(2 * nAVAX),
		},
		{
			name:     "exceed_max_tip",
			newBlock: newBlock(200, newTx(math.MaxUint64)),
			want:     c.MaxSuggestedTip,
		},
		{
			name:     "no_transactions",
			newBlock: newBlock(300),
			want:     c.MaxSuggestedTip, // Defaults to previous prediction
		},
	}
	for i, s := range steps {
		if s.newBlock != nil {
			sut.acceptBlock(s.newBlock)
		}

		got, err := sut.SuggestTipCap(t.Context())
		require.NoErrorf(t, err, "%T.SuggestTipCap(%s (%d))", sut.Estimator, s.name, i)
		require.Equalf(t, s.want, got, "%T.SuggestTipCap(%s (%d))", sut.Estimator, s.name, i)
	}
}

func TestFeeHistory(t *testing.T) {
	c := Config{
		HistoryMaxBlocksFromTip: 1,
		HistoryMaxBlocks:        2,
	}
	sut := newSUT(t, c)

	type args struct {
		numBlocks   uint64
		lastBlock   rpc.BlockNumber
		percentiles []float64
	}
	type results struct {
		height      *big.Int
		rewards     [][]*big.Int
		baseFees    []*big.Int
		portionFull []float64
		err         error
	}
	type test struct {
		name string
		args args
		want results
	}
	bounds := &blocks.WorstCaseBounds{
		NextGasTime: gastime.New(time.Now(), 1, math.MaxUint64),
	}
	newBlock := func(txs ...*types.Transaction) *blocks.Block {
		t.Helper()
		return sut.newBlock(t, 0, bounds, txs...)
	}
	newTx := func(gas, price uint64) *types.Transaction {
		t.Helper()
		return sut.newTx(t, gas, price)
	}
	steps := []struct {
		name     string
		newBlock *blocks.Block
		tests    []test
	}{
		{
			name: "genesis",
			tests: []test{
				{
					name: "too_many_percentiles",
					args: args{
						percentiles: make([]float64, maxPercentiles+1),
					},
					want: results{
						height: common.Big0,
						err:    errBadPercentile,
					},
				},
				{
					name: "percentile_out_of_range",
					args: args{
						percentiles: []float64{-1},
					},
					want: results{
						height: common.Big0,
						err:    errBadPercentile,
					},
				},
				{
					name: "duplicate_percentile",
					args: args{
						percentiles: []float64{1, 1},
					},
					want: results{
						height: common.Big0,
						err:    errBadPercentile,
					},
				},
				{
					name: "future_block",
					args: args{
						lastBlock: 1,
					},
					want: results{
						height: common.Big0,
						err:    errMissingBlock,
					},
				},
				{
					name: "no_blocks",
					args: args{
						lastBlock: rpc.EarliestBlockNumber,
					},
					want: results{
						height: common.Big0,
					},
				},
				{
					name: "missing_worst_case_bounds",
					args: args{
						numBlocks: 1,
						lastBlock: rpc.LatestBlockNumber,
					},
					want: results{
						height: common.Big0,
						err:    errMissingWorstCaseBounds,
					},
				},
			},
		},
		{
			name:     "first_new_block",
			newBlock: newBlock(newTx(21_000, nAVAX)),
			tests: []test{
				{
					name: "query_genesis",
					args: args{
						numBlocks: math.MaxUint64, // capped to prevent overflow
						lastBlock: rpc.EarliestBlockNumber,
					},
					want: results{
						height: common.Big0,
						baseFees: []*big.Int{
							big.NewInt(params.InitialBaseFee),
							big.NewInt(1),
						},
						portionFull: []float64{
							0,
						},
					},
				},
				{
					name: "query_latest",
					args: args{
						numBlocks: 1,
						lastBlock: rpc.LatestBlockNumber,
					},
					want: results{
						height: common.Big1,
						baseFees: []*big.Int{
							big.NewInt(1),
							bounds.NextGasTime.BaseFee().ToBig(),
						},
						portionFull: []float64{
							21_000. / gasLimit,
						},
					},
				},
			},
		},
		{
			name: "second_new_block",
			newBlock: newBlock(
				sut.newTx(t, 100_000, nAVAX),
				sut.newTx(t, 100_000, 2*nAVAX),
				sut.newTx(t, 100_000, 3*nAVAX),
				sut.newTx(t, 100_000, 4*nAVAX),
				sut.newTx(t, 100_000, 5*nAVAX),
			),
			tests: []test{
				{
					name: "query_too_old_block",
					args: args{
						lastBlock: rpc.EarliestBlockNumber,
					},
					want: results{
						height: common.Big0,
						err:    errHistoryDepthExhausted,
					},
				},
				{
					name: "query_max_blocks_with_percentiles",
					args: args{
						numBlocks:   math.MaxUint64, // capped
						lastBlock:   rpc.LatestBlockNumber,
						percentiles: []float64{25, 50, 75},
					},
					want: results{
						height: common.Big1,
						rewards: [][]*big.Int{
							{big.NewInt(nAVAX), big.NewInt(nAVAX), big.NewInt(nAVAX)},
							{big.NewInt(2 * nAVAX), big.NewInt(3 * nAVAX), big.NewInt(4 * nAVAX)},
						},
						baseFees: []*big.Int{
							big.NewInt(1),
							big.NewInt(2),
							bounds.NextGasTime.BaseFee().ToBig(),
						},
						portionFull: []float64{
							21_000. / gasLimit,
							.5,
						},
					},
				},
			},
		},
	}
	for _, s := range steps {
		if s.newBlock != nil {
			sut.acceptBlock(s.newBlock)
		}

		for _, test := range s.tests {
			t.Run(fmt.Sprintf("%s/%s", s.name, test.name), func(t *testing.T) {
				a := test.args
				want := test.want

				height, rewards, baseFees, portionFull, err := sut.FeeHistory(t.Context(), a.numBlocks, a.lastBlock, a.percentiles)
				require.ErrorIs(t, err, want.err)
				assert.Equal(t, want.height, height)
				assert.Equal(t, want.rewards, rewards)
				assert.Equal(t, want.baseFees, baseFees)
				assert.Equal(t, want.portionFull, portionFull)
			})
		}
	}
}
