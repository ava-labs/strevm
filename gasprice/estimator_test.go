// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/saetest"
)

var testChainConfig = saetest.ChainConfig()

// testBlockGen is a simplified block generator for tests, replacing core.BlockGen.
type testBlockGen struct {
	coinbase common.Address
	baseFee  *big.Int
	txs      types.Transactions
	nonces   map[common.Address]uint64
}

func (bg *testBlockGen) SetCoinbase(addr common.Address) {
	bg.coinbase = addr
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
		return 0, fmt.Errorf("%w: block %d", errRequestBeyondHead, n)
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
	if n >= uint64(len(b.blocks)) {
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
			h.Coinbase = bg.coinbase
		}))
		blocks = append(blocks, block)
		parent = block
	}

	return &testBackend{blocks: blocks}
}

type suggestTipCapTest struct {
	numBlocks   int
	genBlock    func(int, *testBlockGen)
	expectedTip *big.Int
}

func defaultEstimatorOptions(t *testing.T) []EstimatorOption {
	blocksOpt, err := WithBlocks(20)
	require.NoError(t, err)
	percentileOpt, err := WithPercentile(60)
	require.NoError(t, err)
	lookbackOpt, err := WithMaxLookbackSeconds(80)
	require.NoError(t, err)

	return []EstimatorOption{
		blocksOpt,
		percentileOpt,
		lookbackOpt,
	}
}

// timeCrunchEstimatorConfig returns a config with [MaxLookbackSeconds] set to 5
// to ensure that during gas price estimation, we will hit the time based look back limit
func timeCrunchEstimatorOptions(t *testing.T) []EstimatorOption {
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
	if test.genBlock == nil {
		test.genBlock = func(i int, b *testBlockGen) {}
	}
	backend := newTestBackend(t, test.numBlocks, test.genBlock)
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
	kc := saetest.NewUNSAFEKeyChain(t, 1)
	addr := kc.Addresses()[0]
	signer := types.LatestSigner(testChainConfig)

	return func(i int, b *testBlockGen) {
		b.SetCoinbase(common.Address{1})

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
	kc := saetest.NewUNSAFEKeyChain(t, 1)
	addr := kc.Addresses()[0]
	signer := types.LatestSigner(testChainConfig)

	return func(i int, b *testBlockGen) {
		b.SetCoinbase(common.Address{1})
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
		expectedTip *big.Int
	}{
		{
			name:        "simple_latest_no_tip",
			numBlocks:   3,
			genBlock:    testGenBlock(t, 0, 80),
			expectedTip: DefaultMinPrice,
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
			expectedTip: DefaultMaxPrice,
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
			expectedTip: DefaultMinPrice,
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
				b.SetCoinbase(common.Address{1})
				// No transactions added
			},
			expectedTip: DefaultMinPrice,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyGasPriceTest(t, suggestTipCapTest{
				numBlocks:   c.numBlocks,
				genBlock:    c.genBlock,
				expectedTip: c.expectedTip,
			}, defaultEstimatorOptions(t)...)
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
