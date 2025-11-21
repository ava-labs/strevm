// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"context"
	"errors"
	"math/big"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/network/p2p/p2ptest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/cmputils"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/state/snapshot.(*diskLayer).generate"),
		// Even with a call to [txpool.TxPool.Close], this leaks. We don't
		// expect to open and close multiple pools, so it's ok to ignore.
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/txpool.(*TxPool).loop.func2"),
	)
}

// SUT is the system under test, primarily the [Set].
type SUT struct {
	*Set
	chain  *blockstest.ChainBuilder
	wallet *saetest.Wallet
	exec   *saexec.Executor
	bloom  *gossip.BloomFilter
}

func chainConfig() *params.ChainConfig {
	return params.AllDevChainProtocolChanges
}

func newWallet(tb testing.TB, numAccounts uint) *saetest.Wallet {
	tb.Helper()
	signer := types.LatestSigner(chainConfig())
	return saetest.NewUNSAFEWallet(tb, numAccounts, signer)
}

func newSUT(t *testing.T, numAccounts uint, existingTxs ...*types.Transaction) SUT {
	t.Helper()
	logger := saetest.NewTBLogger(t, logging.Warn)

	wallet := newWallet(t, numAccounts)
	config := chainConfig()

	db := rawdb.NewMemoryDatabase()
	genesis := blockstest.NewGenesis(t, db, config, saetest.MaxAllocFor(wallet.Addresses()...))
	chain := blockstest.NewChainBuilder(genesis)

	exec, err := saexec.New(genesis, chain.GetBlock, config, db, nil, &hookstest.Stub{Target: 1e6}, logger)
	require.NoError(t, err, "saexec.New()")
	t.Cleanup(func() {
		require.NoErrorf(t, exec.Close(), "%T.Close()", exec)
	})

	bloom, err := gossip.NewBloomFilter(prometheus.NewRegistry(), "", 1, 1e-9, 1e-9)
	require.NoError(t, err, "gossip.NewBloomFilter([1 in a billion FP])")

	bc := NewBlockChain(exec, chain.GetBlock)
	pool := newTxPool(t, bc)
	require.NoErrorf(t, errors.Join(pool.Add(existingTxs, false, true)...), "%T.Add([existing txs in setup], local=false, sync=true)", pool)
	set := NewSet(logger, pool, bloom, 1)
	t.Cleanup(func() {
		assert.NoErrorf(t, set.Close(), "%T.Close()", set)
		assert.NoErrorf(t, pool.Close(), "%T.Close()", pool)
	})

	return SUT{
		Set:    set,
		chain:  chain,
		wallet: wallet,
		exec:   exec,
		bloom:  bloom,
	}
}

func newTxPool(t *testing.T, bc BlockChain) *txpool.TxPool {
	t.Helper()

	config := legacypool.DefaultConfig // copies
	config.Journal = filepath.Join(t.TempDir(), "transactions.rlp")
	subs := []txpool.SubPool{legacypool.New(config, bc)}

	p, err := txpool.New(1, bc, subs)
	require.NoError(t, err, "txpool.New()")
	return p
}

func TestExecutorIntegration(t *testing.T) {
	ctx := t.Context()

	const numAccounts = 3
	s := newSUT(t, numAccounts)

	rng := rand.New(rand.NewPCG(0, 0)) //nolint:gosec // Reproducibility is useful in tests

	const txPerAccount = 5
	const numTxs = numAccounts * txPerAccount
	for range txPerAccount {
		for i := range numAccounts {
			tx := s.wallet.SetNonceAndSign(t, i, &types.DynamicFeeTx{
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasFeeCap: big.NewInt(100),
				GasTipCap: big.NewInt(1 + rng.Int64N(3)),
			})
			require.NoErrorf(t, s.Add(Transaction{tx}), "%T.Add()", s.Set)
		}
	}

	t.Run("Iterate_after_Add", func(t *testing.T) {
		// Note that calls to [txpool.TxPool] are only necessary in tests, and MUST
		// NOT be replicated in production.
		require.NoErrorf(t, s.Pool.Sync(), "%T.Sync()", s.Pool)
		require.Lenf(t, slices.Collect(s.Iterate), numTxs, "slices.Collect(%T.Iterate)", s.Set)
	})
	if t.Failed() {
		t.FailNow()
	}

	var (
		txs  types.Transactions
		last *LazyTransaction
	)
	for _, tx := range s.TransactionsByPriority(txpool.PendingFilter{}) {
		txs = append(txs, tx.Resolve())

		t.Run("priority_ordering", func(t *testing.T) {
			defer func() { last = tx }()

			switch {
			case last == nil:
			case tx.Sender == last.Sender:
				require.Equal(t, last.Tx.Nonce()+1, tx.Tx.Nonce(), "incrementing nonce for same sender")
			case tx.GasTipCap.Eq(last.GasTipCap):
				require.True(t, last.Time.Before(tx.Time), "equal gas tips ordered by first seen")
			default:
				require.Greater(t, last.GasTipCap.Uint64(), tx.GasTipCap.Uint64(), "larger gas tips first")
			}
		})
	}
	require.Lenf(t, txs, numTxs, "%T.TransactionsByPriority()", s.Set)

	b := s.chain.NewBlock(t, txs)
	require.NoErrorf(t, s.exec.Enqueue(ctx, b), "%T.Enqueue([txs from %T.TransactionsByPriority()])", s.exec, s.Set)

	assert.EventuallyWithTf(
		t, func(c *assert.CollectT) {
			require.NoErrorf(c, s.Pool.Sync(), "%T.Sync()", s.Pool)
			assert.Emptyf(c, slices.Collect(s.Iterate), "slices.Collect(%T.Iterate)", s.Set)
			for _, tx := range txs {
				assert.Falsef(c, s.Has(ids.ID(tx.Hash())), "%T.Has(%#x)", s.Set, tx.Hash())
			}
		},
		2*time.Second, 10*time.Millisecond,
		"empty %T after transactions included in a block", s.Set,
	)

	t.Run("block_execution", func(t *testing.T) {
		// The above test for nonce ordering only runs if the same sender has 2
		// consecutive transactions. Successful execution demonstrates correct
		// nonce ordering across the board.
		require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
		for i, r := range b.Receipts() {
			assert.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T[%d].Status", r, i)
		}
	})
}

func TestP2PIntegration(t *testing.T) {
	ctx := t.Context()

	reg := prometheus.NewRegistry()
	metrics, err := gossip.NewMetrics(reg, "")
	require.NoError(t, err, "gossip.NewMetrics()")

	tests := []struct {
		name     string
		gossiper func(_ logging.Logger, sendID ids.NodeID, send *Set, recvID ids.NodeID, recv *Set) (gossip.Gossiper, error)
	}{
		{
			name: "push",
			gossiper: func(l logging.Logger, sendID ids.NodeID, send *Set, recvID ids.NodeID, recv *Set) (gossip.Gossiper, error) {
				c := p2ptest.NewClient(
					t, ctx,
					sendID, p2p.NoOpHandler{},
					recvID, gossip.NewHandler(l, Marshaller{}, recv, metrics, 0),
				)
				branch := gossip.BranchingFactor{Peers: 1}
				return gossip.NewPushGossiper(
					Marshaller{},
					send,
					&stubPeers{[]ids.NodeID{recvID}},
					c,
					metrics,
					branch, branch,
					0, 1<<20, time.Millisecond,
				)
			},
		},
		{
			name: "pull",
			gossiper: func(l logging.Logger, sendID ids.NodeID, send *Set, recvID ids.NodeID, recv *Set) (gossip.Gossiper, error) {
				c := p2ptest.NewClient(
					t, ctx,
					sendID, gossip.NewHandler(l, Marshaller{}, send, metrics, 0),
					recvID, gossip.NewHandler(l, Marshaller{}, recv, metrics, 0),
				)
				return gossip.NewPullGossiper(l, Marshaller{}, recv, c, metrics, 1), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := saetest.NewTBLogger(t, logging.Debug)
			ctx = logger.CancelOnError(ctx)

			sendID := ids.GenerateTestNodeID()
			recvID := ids.GenerateTestNodeID()
			send := newSUT(t, 1)
			// Although the receiving mempool doesn't need to sign transactions, create
			// the same (deterministic) account so it has non-zero balance otherwise the
			// mempool will reject it.
			recv := newSUT(t, 1)

			tx := Transaction{
				Transaction: send.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
					To:       &common.Address{},
					Gas:      params.TxGas,
					GasPrice: big.NewInt(1),
				}),
			}
			require.NoErrorf(t, send.Add(tx), "%T.Add()", send.Set)
			require.NoErrorf(t, send.Pool.Sync(), "sender %T.Sync()", send.Pool)

			gossiper, err := tt.gossiper(logger, sendID, send.Set, recvID, recv.Set)
			require.NoError(t, err, "Bad test setup: gossiper creation failed")
			if push, ok := gossiper.(*gossip.PushGossiper[Transaction]); ok {
				push.Add(tx)
			}
			require.NoErrorf(t, gossiper.Gossip(ctx), "%T.Gossip()", gossiper)

			// The Bloom filter is the deepest in the stack of recipients, going
			// [Set.Add] -> [txpool.TxPool] -> [legacypool.LegacyPool] ->
			// [core.NewTxsEvent] -> [gossip.BloomFilter]. If it has it then
			// everything upstream does too.
			require.Eventuallyf(
				t, func() bool {
					return recv.bloom.Has(tx)
				},
				3*time.Second, 50*time.Millisecond,
				"Receiving %T.bloom.Has(tx)", recv.Set,
			)

			assert.True(t, recv.Has(tx.GossipID()), "receiving %T.Has([tx])", recv.Set)

			want := slices.Collect(send.Iterate)
			got := slices.Collect(recv.Iterate)
			if diff := cmp.Diff(want, got, cmputils.TransactionsByHash()); diff != "" {
				t.Errorf("slices.Collect(%T.Iterate) diff (-sender +receiver):\n%s", send.Set, diff)
			}
		})
	}
}

type stubPeers struct {
	ids []ids.NodeID
}

var _ interface {
	p2p.NodeSampler
	p2p.ValidatorSubset
} = (*stubPeers)(nil)

func (p *stubPeers) Sample(context.Context, int) []ids.NodeID {
	return p.ids
}

func (p *stubPeers) Top(context.Context, float64) []ids.NodeID {
	return p.ids
}

func TestTxPoolPopulatesBloomFilter(t *testing.T) {
	// Note that the wallet addresses are deterministic, so this single address
	// will have sufficient funds when the SUT is created below.
	wallet := newWallet(t, 1)

	var txs types.Transactions
	const n = 10
	for range n {
		txs = append(txs, wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       &common.Address{},
			Gas:      params.TxGas,
			GasPrice: big.NewInt(1),
		}))
	}

	existing := txs[:n/2]
	deferred := txs[n/2:]

	sut := newSUT(t, 1, existing...)
	for i, tx := range existing {
		assert.True(t, sut.bloom.Has(Transaction{tx}), "%T.Has(%#x [tx[%d] already in %T when constructing %T])", sut.bloom, tx.Hash(), i, sut.Pool, sut.Set)
	}

	require.NoError(t, errors.Join(sut.Pool.Add(deferred, false, true)...))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		for i, tx := range deferred {
			idx := i + n/2
			require.Truef(c, sut.bloom.Has(Transaction{tx}), "%T.Has(%#x [tx[%d] added to %T after constructing %T])", sut.bloom, tx.Hash(), idx, sut.Pool, sut.Set)
		}
	}, 2*time.Second, 20*time.Millisecond)
}
