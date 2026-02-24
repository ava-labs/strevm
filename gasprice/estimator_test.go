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

func TestConfigValidate(t *testing.T) {
	modifyDefaultConfig := func(modify func(*Config)) Config {
		cfg := DefaultConfig()
		modify(&cfg)
		return cfg
	}
	tests := []struct {
		name    string
		config  Config
		wantErr error
	}{
		{
			name:    "default_config_valid",
			config:  DefaultConfig(),
			wantErr: nil,
		},
		{
			name:    "nil_Now",
			config:  modifyDefaultConfig(func(c *Config) { c.Now = nil }),
			wantErr: errNilNow,
		},
		{
			name:    "nil_MinSuggestedTip",
			config:  modifyDefaultConfig(func(c *Config) { c.MinSuggestedTip = nil }),
			wantErr: errNilMinSuggestedTip,
		},
		{
			name:    "nil_MaxSuggestedTip",
			config:  modifyDefaultConfig(func(c *Config) { c.MaxSuggestedTip = nil }),
			wantErr: errNilMaxSuggestedTip,
		},
		{
			name:    "SuggestedTipPercentile_zero",
			config:  modifyDefaultConfig(func(c *Config) { c.SuggestedTipPercentile = 0 }),
			wantErr: errBadTipPercentile,
		},
		{
			name:    "SuggestedTipPercentile_above_one",
			config:  modifyDefaultConfig(func(c *Config) { c.SuggestedTipPercentile = 1.1 }),
			wantErr: errBadTipPercentile,
		},
		{
			name: "MinSuggestedTip_exceeds_MaxSuggestedTip",
			config: modifyDefaultConfig(func(c *Config) {
				c.MinSuggestedTip = big.NewInt(200 * params.Wei)
				c.MaxSuggestedTip = big.NewInt(100 * params.Wei)
			}),
			wantErr: errMinTipExceedsMax,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.config.validate(), tt.wantErr)
		})
	}
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

	c.Now = func() time.Time {
		return backend.LastAcceptedBlock().Timestamp()
	}
	log := saetest.NewTBLogger(tb, logging.Debug)
	e, err := NewEstimator(backend, log, c)
	require.NoError(tb, err)
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
	cfg := DefaultConfig()
	cfg.MinSuggestedTip = big.NewInt(aAVAX)
	cfg.MaxSuggestedTip = big.NewInt(avax)
	clk := time.Unix(100, 0)
	cfg.Now = func() time.Time {
		return clk
	}
	nowSec := uint64(clk.Unix())

	type test struct {
		name   string
		blocks func(*testing.T, *SUT) []*blocks.Block
		want   *big.Int
	}
	tests := []test{
		{
			name: "genesis",
			want: cfg.MinSuggestedTip,
		},
		{
			name: "single_tx",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, nAVAX)),
				}
			},
			want: big.NewInt(nAVAX),
		},
		{
			name: "multiple_blocks",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec-10, nil, sut.newTx(t, 1, nAVAX)),
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, 3*nAVAX), sut.newTx(t, 1, 2*nAVAX)),
				}
			},
			want: big.NewInt(nAVAX),
		},
		{
			name: "increase_tip",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec-20, nil, sut.newTx(t, 1, nAVAX)),
					sut.newBlock(t, nowSec-10, nil, sut.newTx(t, 1, 3*nAVAX), sut.newTx(t, 1, 2*nAVAX)),
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, 4*nAVAX)),
				}
			},
			want: big.NewInt(2 * nAVAX),
		},
		{
			name: "min_tip",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, 1)),
				}
			},
			want: cfg.MinSuggestedTip,
		},
		{
			name: "exceed_max_tip",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec-10, nil, sut.newTx(t, 1, math.MaxUint64)),
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, math.MaxUint64)),
				}
			},
			want: cfg.MaxSuggestedTip,
		},
		{
			name: "exceed_max_duration",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, nowSec-(uint64(cfg.SuggestedTipMaxDuration.Seconds())+1), nil, sut.newTx(t, 1, math.MaxUint64), sut.newTx(t, 1, math.MaxUint64), sut.newTx(t, 1, math.MaxUint64)),
					sut.newBlock(t, nowSec, nil, sut.newTx(t, 1, nAVAX)),
				}
			},
			want: big.NewInt(1 * nAVAX),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sut := newSUT(t, cfg)
			var newBlocks []*blocks.Block
			if test.blocks != nil {
				newBlocks = test.blocks(t, sut)
			}
			for _, block := range newBlocks {
				sut.acceptBlock(block)
			}

			got, err := sut.SuggestTipCap(t.Context())
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestFeeHistory(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HistoryMaxBlocksFromTip = 1
	cfg.HistoryMaxBlocks = 2
	bounds := &blocks.WorstCaseBounds{
		NextGasTime: gastime.New(time.Now(), 1, math.MaxUint64),
	}

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
	tests := []struct {
		name   string
		blocks func(*testing.T, *SUT) []*blocks.Block
		args   args
		want   results
	}{
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
		{
			name: "query_genesis",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, 0, bounds, sut.newTx(t, 21_000, nAVAX)),
				}
			},
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
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, 0, bounds, sut.newTx(t, 21_000, nAVAX)),
				}
			},
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
		{
			name: "query_too_old_block",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, 0, bounds, sut.newTx(t, 21_000, nAVAX)),
					sut.newBlock(t, 0, bounds,
						sut.newTx(t, 100_000, nAVAX),
						sut.newTx(t, 100_000, 2*nAVAX),
						sut.newTx(t, 100_000, 3*nAVAX),
						sut.newTx(t, 100_000, 4*nAVAX),
						sut.newTx(t, 100_000, 5*nAVAX),
					),
				}
			},
			args: args{
				lastBlock: rpc.EarliestBlockNumber, // c.HistoryMaxBlocksFromTip is 1
			},
			want: results{
				height: common.Big0,
				err:    errHistoryDepthExhausted,
			},
		},
		{
			name: "query_max_blocks_with_percentiles",
			blocks: func(t *testing.T, sut *SUT) []*blocks.Block {
				return []*blocks.Block{
					sut.newBlock(t, 0, bounds, sut.newTx(t, 21_000, nAVAX)),
					sut.newBlock(t, 0, bounds,
						sut.newTx(t, 100_000, nAVAX),
						sut.newTx(t, 100_000, 2*nAVAX),
						sut.newTx(t, 100_000, 3*nAVAX),
						sut.newTx(t, 100_000, 4*nAVAX),
						sut.newTx(t, 100_000, 5*nAVAX),
					),
				}
			},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sut := newSUT(t, cfg)
			if tt.blocks != nil {
				for _, blk := range tt.blocks(t, sut) {
					sut.acceptBlock(blk)
				}
			}

			a := tt.args
			want := tt.want
			height, rewards, baseFees, portionFull, err := sut.FeeHistory(t.Context(), a.numBlocks, a.lastBlock, a.percentiles)
			require.ErrorIs(t, err, want.err)
			assert.Equal(t, want.height, height)
			assert.Equal(t, want.rewards, rewards)
			assert.Equal(t, want.baseFees, baseFees)
			assert.Equal(t, want.portionFull, portionFull)
		})
	}
}
