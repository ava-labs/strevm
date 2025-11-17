// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"context"
	"math/big"
	"math/rand/v2"
	"path/filepath"
	"runtime"
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

// sut is the system under test, primarily the [Set].
type sut struct {
	*Set
	chain  *blockstest.ChainBuilder
	wallet *saetest.Wallet
	exec   *saexec.Executor
}

func newSUT(t *testing.T, numAccounts uint) sut {
	logger := saetest.NewTBLogger(t, logging.Warn)

	config := params.AllDevChainProtocolChanges
	signer := types.LatestSigner(config)
	wallet := saetest.NewUNSAFEWallet(t, numAccounts, signer)

	db := rawdb.NewMemoryDatabase()
	genesis := blockstest.NewGenesis(t, db, config, saetest.MaxAllocFor(wallet.Addresses()...))

	exec, err := saexec.New(genesis, config, db, nil, &hookstest.Stub{Target: 1e6}, logger)
	require.NoError(t, err, "saexec.New()")
	t.Cleanup(exec.Close)

	bloom, err := gossip.NewBloomFilter(prometheus.NewRegistry(), "", 1, 1e-9, 1e-9)
	require.NoError(t, err, "gossip.NewBloomFilter([1 in a billion FP])")

	chain := blockstest.NewChainBuilder(genesis)
	bc := NewBlockChain(exec, func(h common.Hash, n uint64) *types.Block {
		b, ok := chain.GetBlock(h, n)
		if !ok {
			return nil
		}
		return b.EthBlock()
	})

	set, cleanup := NewSet(logger, newTxPool(t, bc), bloom, 1)
	t.Cleanup(func() {
		require.NoError(t, cleanup(), "cleanup function returned by NewSet()")
	})

	return sut{
		Set:    set,
		chain:  chain,
		wallet: wallet,
		exec:   exec,
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
				require.Equal(t, last.Tx.Nonce()+1, tx.Tx.Nonce())
			case tx.GasTipCap.Eq(last.GasTipCap):
				require.True(t, last.Time.Before(tx.Time))
			default:
				require.GreaterOrEqual(t, last.GasTipCap.Uint64(), tx.GasTipCap.Uint64())
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
		// Although not strictly necessary, this is a secondary check for the
		// maintenance of nonce ordering. It is more reliable than our manual
		// confirmation earlier, so worth the 3 lines.
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
					recvID, gossip.NewHandler(l, Marshaller{}, recv, metrics, 0),
					sendID, gossip.NewHandler(l, Marshaller{}, send, metrics, 0),
				)
				return gossip.NewPullGossiper(l, Marshaller{}, recv, c, metrics, 1), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := saetest.NewTBLogger(t, logging.Debug)

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
			require.NoError(t, err)
			if push, ok := gossiper.(*gossip.PushGossiper[Transaction]); ok {
				push.Add(tx)
			}

			// Instrumenting the receiving [Set] or intercepting the p2p layer
			// is overkill when we can just retry and spin.
			for ; !recv.Has(tx.GossipID()); runtime.Gosched() {
				require.NoErrorf(t, gossiper.Gossip(ctx), "%T.Gossip()", gossiper)
				require.NoErrorf(t, recv.Pool.Sync(), "receiver %T.Sync()", recv.Pool)
			}

			want := slices.Collect(send.Iterate)
			got := slices.Collect(recv.Iterate)
			opt := cmp.Comparer(func(a, b Transaction) bool {
				return a.Hash() == b.Hash()
			})
			if diff := cmp.Diff(want, got, opt); diff != "" {
				t.Errorf("slices.Collect(%T.Iterate) diff (-sender +receiver):\n%s", send.Set, diff)
			}

			require.Eventuallyf(
				t, func() bool {
					return recv.bloom.Has(tx)
				},
				2*time.Second, 50*time.Millisecond,
				"Receiving %T.bloom.Has(tx)", recv.Set,
			)
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
