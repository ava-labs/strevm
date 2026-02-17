// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/saetest"
)

var testChainConfig = saetest.ChainConfig()

// testBlockGen is a simplified block generator for tests, replacing core.BlockGen.
type testBlockGen struct {
	baseFee *big.Int
	txs     types.Transactions
	nonces  map[common.Address]uint64
}

func (bg *testBlockGen) BaseFee() *big.Int {
	return new(big.Int).Set(bg.baseFee)
}

func (bg *testBlockGen) TxNonce(addr common.Address) uint64 {
	n := bg.nonces[addr]
	bg.nonces[addr] = n + 1
	return n
}

func (bg *testBlockGen) AddTx(tx *types.Transaction) {
	bg.txs = append(bg.txs, tx)
}

// testBackend is an in-memory implementation of EstimatorBackend for tests.
type testBackend struct {
	blocks     []*types.Block // blocks[i] is block with number i
	acceptedCh chan<- *types.Block
	pruned     []uint64 // block numbers that return (nil, nil)
}

func (b *testBackend) lastBlock() *types.Block {
	return b.blocks[len(b.blocks)-1]
}

func (b *testBackend) ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := b.lastBlock().Number().Uint64()
	if bn == rpc.PendingBlockNumber {
		return head, nil
	}
	if bn < 0 {
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative checked above
	if n > head {
		return 0, fmt.Errorf("%w: block %d", errFutureBlock, n)
	}
	return n, nil
}

func (b *testBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	block, err := b.BlockByNumber(ctx, number)
	if err != nil || block == nil {
		return nil, err
	}
	return block.Header(), nil
}

func (b *testBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.PendingBlockNumber {
		return b.lastBlock(), nil
	}
	n := uint64(number) //nolint:gosec // Test code
	if n >= uint64(len(b.blocks)) || slices.Contains(b.pruned, n) {
		return nil, nil
	}
	return b.blocks[n], nil
}

func (b *testBackend) SubscribeChainAcceptedEvent(ch chan<- *types.Block) event.Subscription {
	b.acceptedCh = ch
	return event.NewSubscription(func(quit <-chan struct{}) error {
		<-quit
		return nil
	})
}

// newTestBackend creates a test backend with [numBlocks] blocks (plus genesis).
// The [genBlock] function is called for each non-genesis block to populate it
// with transactions.
func newTestBackend(t *testing.T, numBlocks int, genBlock func(int, *testBlockGen)) *testBackend {
	t.Helper()

	baseFee := big.NewInt(875_000_000) // ~0.875 GWei

	// Create genesis block (block 0)
	genesis := types.NewBlock(&types.Header{
		Number:   big.NewInt(0),
		BaseFee:  new(big.Int).Set(baseFee),
		Time:     0,
		GasLimit: 8_000_000,
	}, nil, nil, nil, saetest.TrieHasher())

	blocks := make([]*types.Block, 0, numBlocks+1)
	blocks = append(blocks, genesis)

	nonces := make(map[common.Address]uint64)
	parent := genesis
	for i := 0; i < numBlocks; i++ {
		bg := &testBlockGen{
			baseFee: new(big.Int).Set(baseFee),
			nonces:  nonces,
		}
		if genBlock != nil {
			genBlock(i, bg)
		}

		var gasUsed uint64
		for _, tx := range bg.txs {
			gasUsed += tx.Gas()
		}

		block := blockstest.NewEthBlock(parent, bg.txs, blockstest.ModifyHeader(func(h *types.Header) {
			h.BaseFee = new(big.Int).Set(baseFee)
			h.Time = parent.Time() + 2
			h.GasLimit = 8_000_000
			h.GasUsed = gasUsed
		}))
		blocks = append(blocks, block)
		parent = block
	}

	return &testBackend{blocks: blocks}
}

type suggestTipCapTest struct {
	numBlocks   int
	genBlock    func(int, *testBlockGen)
	pruned      []uint64
	expectedTip *big.Int
}

// timeCrunchEstimatorConfig returns a config with [MaxLookbackSeconds] set to 5
// to ensure that during gas price estimation, we will hit the time based look back limit
func timeCrunchEstimatorOptions(t *testing.T) []EstimatorOption {
	t.Helper()
	blocksOpt, err := WithBlocks(20)
	require.NoError(t, err)
	percentileOpt, err := WithPercentile(60)
	require.NoError(t, err)
	lookbackOpt, err := WithMaxLookbackSeconds(5)
	require.NoError(t, err)

	return []EstimatorOption{
		blocksOpt,
		percentileOpt,
		lookbackOpt,
	}
}

func applyGasPriceTest(t *testing.T, test suggestTipCapTest, opts ...EstimatorOption) {
	t.Helper()
	if test.genBlock == nil {
		test.genBlock = func(i int, b *testBlockGen) {}
	}
	backend := newTestBackend(t, test.numBlocks, test.genBlock)
	backend.pruned = test.pruned
	estimator, err := NewEstimator(backend, opts...)
	require.NoError(t, err)
	defer estimator.Close()

	// mock time to be consistent across different CI runs
	// sets currentTime to be 20 seconds
	estimator.clock.Set(time.Unix(20, 0))

	got, err := estimator.SuggestTipCap(context.Background())
	require.NoError(t, err)

	if got.Cmp(test.expectedTip) != 0 {
		t.Fatalf("Expected tip (%d), got tip (%d)", test.expectedTip, got)
	}
}

func testGenBlock(t *testing.T, tip int64, numTx int) func(int, *testBlockGen) {
	t.Helper()
	kc := saetest.NewUNSAFEKeyChain(t, 1)
	addr := kc.Addresses()[0]
	signer := types.LatestSigner(testChainConfig)

	return func(i int, b *testBlockGen) {
		txTip := big.NewInt(tip * params.GWei)
		baseFee := b.BaseFee()
		feeCap := new(big.Int).Add(baseFee, txTip)
		for j := 0; j < numTx; j++ {
			tx := kc.SignTx(t, signer, 0, &types.DynamicFeeTx{
				ChainID:   testChainConfig.ChainID,
				Nonce:     b.TxNonce(addr),
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasFeeCap: feeCap,
				GasTipCap: txTip,
				Data:      []byte{},
			})
			b.AddTx(tx)
		}
	}
}

func testGenBlockWithTips(t *testing.T, tips []int64) func(int, *testBlockGen) {
	t.Helper()
	kc := saetest.NewUNSAFEKeyChain(t, 1)
	addr := kc.Addresses()[0]
	signer := types.LatestSigner(testChainConfig)

	return func(i int, b *testBlockGen) {
		baseFee := b.BaseFee()
		for _, tip := range tips {
			txTip := big.NewInt(tip * params.GWei)
			feeCap := new(big.Int).Add(baseFee, txTip)
			tx := kc.SignTx(t, signer, 0, &types.DynamicFeeTx{
				ChainID:   testChainConfig.ChainID,
				Nonce:     b.TxNonce(addr),
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasFeeCap: feeCap,
				GasTipCap: txTip,
				Data:      []byte{},
			})
			b.AddTx(tx)
		}
	}
}

func TestSuggestTipCap(t *testing.T) {
	cases := []struct {
		name        string
		numBlocks   int
		genBlock    func(int, *testBlockGen)
		pruned      []uint64
		expectedTip *big.Int
	}{
		{
			name:        "simple_latest_no_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 0, 80),
			expectedTip: defaultConfig().MinPrice,
		},
		{
			name:        "simple_latest_1_gwei_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 1, 80),
			expectedTip: big.NewInt(1 * params.GWei),
		},
		{
			name:        "simple_latest_100_gwei_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 100, 80),
			expectedTip: big.NewInt(100 * params.GWei),
		},
		{
			name:        "simple_floor_latest_1_gwei_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 1, 80),
			expectedTip: big.NewInt(1 * params.GWei),
		},
		{
			name:        "simple_floor_latest_100_gwei_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 100, 80),
			expectedTip: big.NewInt(100 * params.GWei),
		},
		{
			name:        "max_tip_cap",
			numBlocks:   200,
			genBlock:    testGenBlock(t, 550, 80),
			expectedTip: defaultConfig().MaxPrice,
		},
		{
			name:        "single_transaction_with_tip",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{100}),
			expectedTip: big.NewInt(100 * params.GWei),
		},
		{
			name:        "three_transactions_with_odd_count_tips",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{10, 20, 30}),
			expectedTip: big.NewInt(20 * params.GWei),
		},
		{
			name:        "four_transactions_with_even_count_tips",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{10, 20, 30, 40}),
			expectedTip: big.NewInt(30 * params.GWei),
		},
		{
			name:        "unsorted_transactions_with_tips",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{50, 10, 40, 30, 20}),
			expectedTip: big.NewInt(30 * params.GWei),
		},
		{
			name:        "zero_tips",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{0, 0, 0}),
			expectedTip: defaultConfig().MinPrice,
		},
		{
			name:        "duplicate_tips",
			numBlocks:   3,
			genBlock:    testGenBlockWithTips(t, []int64{20, 20, 20}),
			expectedTip: big.NewInt(20 * params.GWei),
		},
		{
			name:      "no_transactions",
			numBlocks: 3,
			genBlock: func(i int, b *testBlockGen) {
				// No transactions added
			},
			expectedTip: defaultConfig().MinPrice,
		},
		{
			// 5 blocks, each with 1 tx. Tips by block number:
			// 1: 10 GWei, 2: 20 GWei, 3: 30 GWei, 4: 40 GWei, 5: 50 GWei.
			// Without pruning: sorted [10,20,30,40,50], 60th pct → index 2 → 30 GWei.
			// Prune block 5 (50 GWei): sorted [10,20,30,40], 60th pct → index 1 → 20 GWei.
			name:      "pruned_blocks_skipped",
			numBlocks: 5,
			genBlock: func(i int, b *testBlockGen) {
				kc := saetest.NewUNSAFEKeyChain(t, 1)
				addr := kc.Addresses()[0]
				signer := types.LatestSigner(testChainConfig)
				txTip := big.NewInt(int64(i+1) * 10 * params.GWei)
				baseFee := b.BaseFee()
				feeCap := new(big.Int).Add(baseFee, txTip)
				tx := kc.SignTx(t, signer, 0, &types.DynamicFeeTx{
					ChainID:   testChainConfig.ChainID,
					Nonce:     b.TxNonce(addr),
					To:        &common.Address{},
					Gas:       params.TxGas,
					GasFeeCap: feeCap,
					GasTipCap: txTip,
					Data:      []byte{},
				})
				b.AddTx(tx)
			},
			pruned:      []uint64{5},
			expectedTip: big.NewInt(20 * params.GWei),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyGasPriceTest(t, suggestTipCapTest{
				numBlocks:   c.numBlocks,
				genBlock:    c.genBlock,
				pruned:      c.pruned,
				expectedTip: c.expectedTip,
			})
		})
	}
}

func TestSuggestTipCapMaxBlocksSecondsLookback(t *testing.T) {
	applyGasPriceTest(t, suggestTipCapTest{
		numBlocks:   20,
		genBlock:    testGenBlock(t, 55, 80),
		expectedTip: big.NewInt(55 * params.GWei),
	}, timeCrunchEstimatorOptions(t)...)
}

func TestFeeHistory(t *testing.T) {
	cases := []struct {
		maxCallBlock uint64
		maxBlock     uint64
		count        uint64
		last         rpc.BlockNumber
		percent      []float64
		pruned       []uint64
		expFirst     uint64
		expCount     int
		expErr       error
	}{
		// Standard libevm tests
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: nil, expFirst: 21, expCount: 10, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: []float64{0, 10}, expFirst: 21, expCount: 10, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 30, percent: []float64{20, 10}, expFirst: 0, expCount: 0, expErr: errBadPercentile},
		{maxCallBlock: 0, maxBlock: 1000, count: 1000000000, last: 30, percent: nil, expFirst: 0, expCount: 31, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 1000000000, last: rpc.PendingBlockNumber, percent: nil, expFirst: 0, expCount: 33, expErr: nil},
		{maxCallBlock: 0, maxBlock: 1000, count: 10, last: 40, percent: nil, expFirst: 0, expCount: 0, expErr: errFutureBlock},
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: rpc.PendingBlockNumber, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: 32, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},
		// In SAE backend, `pending` resolves to accepted head (not head+1).
		{maxCallBlock: 0, maxBlock: 1000, count: 1, last: rpc.PendingBlockNumber, percent: nil, expFirst: 32, expCount: 1, expErr: nil},
		// With count=2, pending spans [head-1, head].
		{maxCallBlock: 0, maxBlock: 1000, count: 2, last: rpc.PendingBlockNumber, percent: nil, expFirst: 31, expCount: 2, expErr: nil},
		// Reward percentiles should not alter the pending range semantics.
		{maxCallBlock: 0, maxBlock: 1000, count: 2, last: rpc.PendingBlockNumber, percent: []float64{0, 10}, expFirst: 31, expCount: 2, expErr: nil},

		// Modified tests
		{maxCallBlock: 0, maxBlock: 2, count: 100, last: rpc.PendingBlockNumber, percent: nil, expFirst: 31, expCount: 2, expErr: nil},    // apply block lookback limits even if only headers required
		{maxCallBlock: 0, maxBlock: 10, count: 10, last: 30, percent: nil, expFirst: 23, expCount: 8, expErr: nil},                        // limit lookback based on maxHistory from latest block
		{maxCallBlock: 0, maxBlock: 33, count: 1000000000, last: 10, percent: nil, expFirst: 0, expCount: 11, expErr: nil},                // handle truncation edge case
		{maxCallBlock: 0, maxBlock: 2, count: 10, last: 20, percent: nil, expFirst: 0, expCount: 0, expErr: errHistoryDepthExhausted},     // query behind historical limit
		{maxCallBlock: 10, maxBlock: 30, count: 100, last: rpc.PendingBlockNumber, percent: nil, expFirst: 23, expCount: 10, expErr: nil}, // ensure [MaxCallBlockHistory] is honored

		// Pruned block tests
		{maxCallBlock: 0, maxBlock: 1000, count: 5, last: 7, percent: []float64{50}, pruned: []uint64{5}, expFirst: 3, expCount: 2, expErr: nil}, // pruned in middle truncates
		{maxCallBlock: 0, maxBlock: 1000, count: 3, last: 5, percent: []float64{50}, pruned: []uint64{3}, expFirst: 0, expCount: 0, expErr: nil}, // first block pruned returns empty
		{maxCallBlock: 0, maxBlock: 1000, count: 3, last: 7, percent: nil, pruned: []uint64{7}, expFirst: 5, expCount: 2, expErr: nil},           // last block pruned
	}
	for i, c := range cases {
		kc := saetest.NewUNSAFEKeyChain(t, 1)
		addr := kc.Addresses()[0]
		signer := types.LatestSigner(testChainConfig)
		tip := big.NewInt(1 * params.GWei)
		backend := newTestBackend(t, 32, func(i int, b *testBlockGen) {
			baseFee := b.BaseFee()
			feeCap := new(big.Int).Add(baseFee, tip)

			tx := kc.SignTx(t, signer, 0, &types.DynamicFeeTx{
				ChainID:   testChainConfig.ChainID,
				Nonce:     b.TxNonce(addr),
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasFeeCap: feeCap,
				GasTipCap: tip,
				Data:      []byte{},
			})
			b.AddTx(tx)
		})
		backend.pruned = c.pruned
		estimatorOpts := make([]EstimatorOption, 0, 2)
		if c.maxCallBlock != 0 {
			maxCallBlockOpt, err := WithMaxCallBlockHistory(c.maxCallBlock)
			require.NoError(t, err)
			estimatorOpts = append(estimatorOpts, maxCallBlockOpt)
		}
		if c.maxBlock != 0 {
			maxBlockOpt, err := WithMaxBlockHistory(c.maxBlock)
			require.NoError(t, err)
			estimatorOpts = append(estimatorOpts, maxBlockOpt)
		}
		estimator, err := NewEstimator(backend, estimatorOpts...)
		require.NoError(t, err)
		defer estimator.Close()

		first, reward, baseFee, ratio, err := estimator.FeeHistory(context.Background(), c.count, c.last, c.percent)
		expReward := c.expCount
		if len(c.percent) == 0 {
			expReward = 0
		}
		expBaseFee := c.expCount

		assert.Equal(t, c.expFirst, first.Uint64(), "case %d: first block", i)
		assert.Len(t, reward, expReward, "case %d: reward length", i)
		assert.Len(t, baseFee, expBaseFee, "case %d: baseFee length", i)
		assert.Len(t, ratio, c.expCount, "case %d: gasUsedRatio length", i)
		assert.ErrorIs(t, err, c.expErr, "case %d: error", i)
	}
}
