// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math/big"
	"math/rand/v2"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook/hookstest"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/state/snapshot.(*diskLayer).generate"),
		// TxPool.Close() doesn't wait for its loop() method to signal termination.
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/txpool.(*TxPool).loop.func2"),
	)
}

// SUT is the system under test. Testing SHOULD be performed via the embedded
// types as these most accurately reflect the public API. Any access to the
// other fields SHOULD instead be exposed as methods, such as [SUT.stateAt], to
// avoid over-reliance on internal implementation details.
type SUT struct {
	block.ChainVM
	*ethclient.Client

	rawVM   *VM
	genesis *blocks.Block
	wallet  *saetest.Wallet
	db      ethdb.Database
}

type (
	sutConfig struct {
		vmConfig       Config
		hooks          *hookstest.Stub
		genesisOptions []blockstest.GenesisOption
	}
	sutOption = options.Option[sutConfig]
)

func newSUT(tb testing.TB, numAccounts uint, opts ...sutOption) (context.Context, *SUT) {
	tb.Helper()

	mempoolConf := legacypool.DefaultConfig // copies
	mempoolConf.Journal = "/dev/null"

	conf := options.ApplyTo(&sutConfig{
		vmConfig: Config{
			MempoolConfig: mempoolConf,
		},
		hooks: &hookstest.Stub{
			Target: 100e6,
		},
		genesisOptions: []blockstest.GenesisOption{
			blockstest.WithTimestamp(saeparams.TauSeconds),
		},
	}, opts...)

	vm := NewVM(conf.vmConfig)
	snow := adaptor.Convert(&SinceGenesis{VM: vm})
	tb.Cleanup(func() {
		ctx := context.WithoutCancel(tb.Context())
		require.NoError(tb, snow.Shutdown(ctx), "Shutdown()")
	})

	config := saetest.ChainConfig()
	signer := types.LatestSigner(config)
	wallet := saetest.NewUNSAFEWallet(tb, numAccounts, signer)

	db := rawdb.NewMemoryDatabase()
	genesis := blockstest.NewGenesis(tb, db, config, saetest.MaxAllocFor(wallet.Addresses()...), conf.genesisOptions...)

	logger := saetest.NewTBLogger(tb, logging.Warn)
	ctx := logger.CancelOnError(tb.Context())
	snowCtx := snowtest.Context(tb, ids.GenerateTestID())
	snowCtx.Log = logger

	// TODO(arr4n) change this to use [SinceGenesis.Initialize] via the `snow` variable.
	require.NoError(tb, vm.Init(snowCtx, conf.hooks, config, db, &triedb.Config{}, genesis), "Init()")
	_ = snow.Initialize

	handlers, err := snow.CreateHandlers(ctx)
	require.NoErrorf(tb, err, "%T.CreateHandlers()", snow)
	server := httptest.NewServer(handlers[rpcHTTPExtensionPath])
	tb.Cleanup(server.Close)
	client, err := ethclient.Dial(server.URL)
	require.NoError(tb, err, "ethclient.Dial(http.NewServer(%T.CreateHandlers()))", snow)
	tb.Cleanup(client.Close)

	return ctx, &SUT{
		ChainVM: snow,
		Client:  client,
		rawVM:   vm,
		genesis: genesis,
		wallet:  wallet,
		db:      db,
	}
}

// stubbedTime returns an option to configure a new SUT's "now" function along
// with a function to set the time.
func stubbedTime() (_ sutOption, setTime func(time.Time)) {
	var now time.Time
	set := func(n time.Time) {
		now = n
	}
	opt := options.Func[sutConfig](func(c *sutConfig) {
		// TODO(StephenButtolph) unify the time functions provided in the config
		// and the hooks.
		c.vmConfig.Now = func() time.Time {
			return now
		}
		c.hooks.Now = func() uint64 {
			return unix(now)
		}
	})

	return opt, set
}

func withGenesisOpts(opts ...blockstest.GenesisOption) sutOption {
	return options.Func[sutConfig](func(c *sutConfig) {
		c.genesisOptions = append(c.genesisOptions, opts...)
	})
}

func (s *SUT) mustSendTx(tb testing.TB, tx *types.Transaction) {
	tb.Helper()
	require.NoErrorf(tb, s.Client.SendTransaction(tb.Context(), tx), "%T.SendTransaction([%#x])", s.Client, tx.Hash())
}

func (s *SUT) stateAt(tb testing.TB, root common.Hash) *state.StateDB {
	tb.Helper()
	sdb, err := state.New(root, s.rawVM.exec.StateCache(), nil)
	require.NoErrorf(tb, err, "state.New(%#x, %T.StateCache())", root, s.rawVM.exec)
	return sdb
}

// syncMempool is a convenience wrapper for calling [txpool.TxPool.Sync], which
// MUST NOT be done in production.
func (s *SUT) syncMempool(tb testing.TB) {
	tb.Helper()
	var _ txpool.TxPool // maintain import for [comment] rendering
	p := s.rawVM.mempool.Pool
	require.NoErrorf(tb, p.Sync(), "%T.Sync()", p)
}

// runConsensusLoop sets the preference to the specified block then builds,
// verifies, accepts, and returns the new block. It does NOT wait for it to be
// executed; to do this automatically, set the [VM] to [snow.Bootstrapping].
func (s *SUT) runConsensusLoop(tb testing.TB, preference *blocks.Block) *blocks.Block {
	tb.Helper()

	ctx := tb.Context()
	require.NoError(tb, s.SetPreference(ctx, preference.ID()), "SetPreference()")

	proposed, err := s.BuildBlock(ctx)
	require.NoError(tb, err, "BuildBlock()")
	// Ensure that a peer would be able to perform the consensus loop by parsing
	// the block.
	b, err := s.ParseBlock(ctx, proposed.Bytes())
	require.NoError(tb, err, "ParseBlock(BuildBlock().Bytes())")

	require.NoErrorf(tb, b.Verify(ctx), "%T.Verify()", b)
	require.NoErrorf(tb, b.Accept(ctx), "%T.Accept()", b)

	return s.lastAcceptedBlock(tb)
}

// waitUntilExecuted blocks until an external indicator shows that `b` has been
// executed.
func (s *SUT) waitUntilExecuted(tb testing.TB, b *blocks.Block) {
	tb.Helper()
	defer func() {
		tb.Helper()
		require.True(tb, b.Executed(), "%T.Executed()", b)
	}()

	// The subscription is opened before checking the block number to avoid
	// missing the notification that the block was executed.
	c := make(chan core.ChainHeadEvent)
	sub := s.rawVM.exec.SubscribeChainHeadEvent(c)
	defer sub.Unsubscribe()

	ctx := tb.Context()
	num, err := s.Client.BlockNumber(ctx)
	require.NoErrorf(tb, err, "%T.BlockNumber()", s.Client)
	if num >= b.Height() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			tb.Fatalf("waiting for block %d to execute: %v", b.Height(), ctx.Err())
		case err := <-sub.Err():
			require.NoErrorf(tb, err, "%T.SubscribeChainHeadEvent().Err()", s.rawVM.exec)
		case ev := <-c:
			if ev.Block.NumberU64() >= b.Height() {
				return
			}
		}
	}
}

// lastAcceptedBlock is a convenience wrapper for calling [VM.GetBlock] with
// the ID from [VM.LastAccepted] as an argument.
func (s *SUT) lastAcceptedBlock(tb testing.TB) *blocks.Block {
	tb.Helper()
	ctx := tb.Context()
	id, err := s.LastAccepted(ctx)
	require.NoError(tb, err, "LastAccepted()")
	b, err := s.GetBlock(ctx, id)
	require.NoError(tb, err, "GetBlock(lastAcceptedID)")
	return unwrap(tb, b)
}

// unwrap is a convenience (un)wrapper for calling [adaptor.Block.Unwrap] after
// confirming the concrete type of `b`.
func unwrap(tb testing.TB, b snowman.Block) *blocks.Block {
	tb.Helper()
	switch b := b.(type) {
	case adaptor.Block[*blocks.Block]:
		return b.Unwrap()
	default:
		tb.Fatalf("snowman.Block of concrete type %T", b)
		return nil
	}
}

// assertBlockHashInvariants MUST NOT be called concurrently with
// [VM.AcceptBlock] as it depends on the last-accepted block. It also blocks
// until said block has finished execution.
func (s *SUT) assertBlockHashInvariants(ctx context.Context, t *testing.T) {
	t.Helper()
	t.Run("block_hash_invariants", func(t *testing.T) {
		b := s.lastAcceptedBlock(t)
		// The API client is an external reader, so we must wait on an external
		// indicator. The block's WaitUntilExecuted is only an internal
		// indicator.
		s.waitUntilExecuted(t, b)
		t.Logf("Last accepted (and executed) block: %d", b.Height())

		for num, want := range map[rpc.BlockNumber]common.Hash{
			rpc.BlockNumber(b.Number().Int64()): b.Hash(),
			rpc.LatestBlockNumber:               b.Hash(),               // Because we've waited until it's executed
			rpc.SafeBlockNumber:                 b.LastSettled().Hash(), // Safe from disk corruption, not re-org, as acceptance guarantees finality
			rpc.FinalizedBlockNumber:            b.LastSettled().Hash(), // Because we maintain label monotonicity
		} {
			t.Run(num.String(), func(t *testing.T) {
				got, err := s.Client.HeaderByNumber(ctx, big.NewInt(num.Int64()))
				require.NoErrorf(t, err, "%T.HeaderByNumber(%v)", s.Client, num)
				assert.Equalf(t, want, got.Hash(), "%T.HeaderByNumber(%v).Hash()", s.Client, num)
			})
		}

		// The RPC implementation doesn't use the database to resolve the block
		// labels above, so we still need to check them.
		assert.Equal(t, b.Hash(), rawdb.ReadHeadBlockHash(s.db), "rawdb.ReadHeadBlockHash() MUST reflect last-executed block")
		assert.Equal(t, b.LastSettled().Hash(), rawdb.ReadFinalizedBlockHash(s.db), "rawdb.ReadFinalizedBlockHash() MUST reflect last-settled block")
	})
}

func TestIntegration(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	testWaitForEvent := func(t *testing.T) {
		t.Helper()
		ev, err := sut.WaitForEvent(ctx)
		require.NoError(t, err)
		require.Equal(t, snowcommon.PendingTxs, ev)
	}

	waitForEvDone := make(chan struct{})
	go func() {
		defer close(waitForEvDone)
		t.Run("WaitForEvent_early_unblocks", testWaitForEvent)
	}()

	transfer := uint256.NewInt(42)
	recipient := common.Address{1, 2, 3, 4}
	const numTxs = 2
	for range numTxs {
		tx := sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &recipient,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
			Value:     transfer.ToBig(),
		})
		sut.mustSendTx(t, tx)
	}

	select {
	case <-waitForEvDone:
	case <-time.After(time.Second):
		t.Error("WaitForEvent() called before SendTx() did not unblock")
	}

	// Each tx sent to the VM can unblock at most 1 call to [VM.WaitForEvent] so
	// making an extra call proves that it returns due to pending txs already in
	// the mempool. Yes, the one above would be a sufficient extra, but it's
	// best to keep this logic self-contained, should the unblocking test be
	// removed.
	for range numTxs + 1 {
		t.Run("WaitForEvent_with_existing_txs", testWaitForEvent)
	}

	sut.syncMempool(t) // technically we've only proven 1 tx added, as unlikely as a race is
	require.Equal(t, numTxs, sut.rawVM.numPendingTxs(), "number of pending txs")

	b := sut.runConsensusLoop(t, sut.genesis)
	assert.Equal(t, sut.genesis.ID(), b.Parent(), "BuildBlock() builds on preference")
	require.Lenf(t, b.Transactions(), numTxs, "%T.Transactions()", b)

	require.NoError(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	require.Lenf(t, b.Receipts(), numTxs, "%T.Receipts()", b)
	for i, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Receipts()[%d].Status", i, b)
	}

	t.Run("no_tx_replay", func(t *testing.T) {
		// If the tx-inclusion logic were broken then this would include the
		// transactions again, resulting in a FATAL in the execution loop due to
		// non-increasing nonce.
		b := sut.runConsensusLoop(t, b)
		assert.Emptyf(t, b.Transactions(), "%T.Transactions()", b)
		require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	})

	t.Run("post_execution_state", func(t *testing.T) {
		sdb := sut.stateAt(t, b.PostExecutionStateRoot())

		got := sdb.GetBalance(recipient)
		want := new(uint256.Int).Mul(transfer, uint256.NewInt(numTxs))
		require.Equalf(t, want, got, "%T.GetBalance(...)", sdb)
	})
}

func TestSyntacticBlockChecks(t *testing.T) {
	ctx, sut := newSUT(t, 0)

	const now = 1e6
	sut.rawVM.config.Now = func() time.Time {
		return time.Unix(now, 0)
	}

	tests := []struct {
		name    string
		header  *types.Header
		wantErr error
	}{
		{
			name: "block_height_overflow_protection",
			header: &types.Header{
				Number: new(big.Int).Lsh(big.NewInt(1), 64),
			},
			wantErr: errBlockHeightNotUint64,
		},
		{
			name: "block_time_overflow_protection",
			header: &types.Header{
				Number: big.NewInt(1),
				Time:   now + maxBlockFutureSeconds + 1,
			},
			wantErr: errBlockTooFarInFuture,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := blockstest.NewBlock(t, types.NewBlockWithHeader(tt.header), nil, nil)
			_, err := sut.ParseBlock(ctx, b.Bytes())
			assert.ErrorIs(t, err, tt.wantErr, "ParseBlock(#%v @ time %v) when stubbed time is %d", tt.header.Number, tt.header.Time, uint64(now))
		})
	}
}

func TestAcceptBlock(t *testing.T) {
	for blocks.InMemoryBlockCount() != 0 {
		runtime.GC()
	}

	opt, setTime := stubbedTime()
	var now time.Time
	fastForward := func(by time.Duration) {
		now = now.Add(by)
		setTime(now)
	}
	fastForward(saeparams.Tau)

	ctx, sut := newSUT(t, 1, opt)
	// Causes [VM.AcceptBlock] to wait until the block has executed.
	require.NoError(t, sut.SetState(ctx, snow.Bootstrapping), "SetState(Bootstrapping)")

	unsettled := []*blocks.Block{sut.genesis}
	last := func() *blocks.Block {
		return unsettled[len(unsettled)-1]
	}
	sut.genesis = nil // allow it to be GCd when appropriate

	rng := rand.New(rand.NewPCG(0, 0)) //nolint:gosec // Reproducibility is useful for tests
	for range 100 {
		ffMillis := 100 + rng.IntN(1000*(1+saeparams.TauSeconds))
		fastForward(time.Millisecond * time.Duration(ffMillis))

		b := sut.runConsensusLoop(t, last())
		unsettled = append(unsettled, b)
		sut.assertBlockHashInvariants(ctx, t)

		lastSettled := b.LastSettled().Height()
		var wantInMemory set.Set[uint64]
		for i, bb := range unsettled {
			switch {
			case bb == nil: // settled earlier
			case bb.Settled():
				unsettled[i] = nil
				require.LessOrEqual(t, bb.Height(), lastSettled, "height of settled block")

			default:
				require.Greater(t, bb.Height(), lastSettled, "height of unsettled block")
				wantInMemory.Add(
					bb.Height(),
					bb.ParentBlock().Height(),
					bb.LastSettled().Height(),
				)
			}
		}

		assert.EventuallyWithT(t, func(t *assert.CollectT) {
			runtime.GC()
			require.Equal(t, int64(wantInMemory.Len()), blocks.InMemoryBlockCount(), "in-memory block count")
		}, 100*time.Millisecond, time.Millisecond)
	}
}

func TestSemanticBlockChecks(t *testing.T) {
	const now = 1e6
	ctx, sut := newSUT(t, 1, withGenesisOpts(blockstest.WithTimestamp(now)))
	sut.rawVM.config.Now = func() time.Time {
		return time.Unix(now, 0)
	}

	lastAccepted := sut.lastAcceptedBlock(t)
	tests := []struct {
		name           string
		parentHash     common.Hash // defaults to lastAccepted Hash if zero
		acceptedHeight bool        // if true, block height == lastAccepted.Height(); else +1
		time           uint64      // defaults to `now` if zero
		receipts       types.Receipts
		wantErr        error
	}{
		{
			name:       "unknown_parent",
			parentHash: common.Hash{1},
			wantErr:    errUnknownParent,
		},
		{
			name:           "already_finalized",
			acceptedHeight: true,
			wantErr:        errBlockHeightTooLow,
		},
		{
			name:    "block_time_under_minimum",
			time:    saeparams.TauSeconds - 1,
			wantErr: errBlockTimeUnderMinimum,
		},
		{
			name:    "block_time_before_parent",
			time:    lastAccepted.BuildTime() - 1,
			wantErr: errBlockTimeBeforeParent,
		},
		{
			name:    "block_time_after_maximum",
			time:    now + uint64(maxFutureBlockTime.Seconds()) + 1,
			wantErr: errBlockTimeAfterMaximum,
		},
		{
			name: "hash_mismatch",
			receipts: types.Receipts{
				&types.Receipt{}, // Unexpected receipt
			},
			wantErr: errHashMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.parentHash == (common.Hash{}) {
				tt.parentHash = common.Hash(lastAccepted.ID())
			}
			height := lastAccepted.Height()
			if !tt.acceptedHeight {
				height++
			}
			if tt.time == 0 {
				tt.time = now
			}

			ethB := types.NewBlock(
				&types.Header{
					ParentHash: tt.parentHash,
					Number:     new(big.Int).SetUint64(height),
					Time:       tt.time,
				},
				nil, // txs
				nil, // uncles
				tt.receipts,
				saetest.TrieHasher(),
			)
			b := blockstest.NewBlock(t, ethB, nil, nil)
			snowB, err := sut.ParseBlock(ctx, b.Bytes())
			require.NoErrorf(t, err, "ParseBlock(...)")
			require.ErrorIs(t, snowB.Verify(ctx), tt.wantErr, "Verify()")
		})
	}
}
