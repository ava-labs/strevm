// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook/hookstest"
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
}

func newSUT(
	tb testing.TB,
	numAccounts uint,
	genesisOptions ...blockstest.GenesisOption,
) (context.Context, *SUT) {
	tb.Helper()

	mempoolConf := legacypool.DefaultConfig // copies
	mempoolConf.Journal = "/dev/null"

	vm := NewVM(Config{
		MempoolConfig: mempoolConf,
	})
	snow := adaptor.Convert(&SinceGenesis{VM: vm})
	tb.Cleanup(func() {
		ctx := context.WithoutCancel(tb.Context())
		require.NoError(tb, snow.Shutdown(ctx), "Shutdown()")
	})

	config := saetest.ChainConfig()
	signer := types.LatestSigner(config)
	wallet := saetest.NewUNSAFEWallet(tb, numAccounts, signer)

	db := rawdb.NewMemoryDatabase()
	genesis := blockstest.NewGenesis(tb, db, config, saetest.MaxAllocFor(wallet.Addresses()...), genesisOptions...)

	// TODO(StephenButtolph) unify the time function provided in the config and
	// the hooks.
	hooks := &hookstest.Stub{
		Now: func() uint64 {
			return uint64(vm.config.Now().Unix()) //nolint:gosec // Time won't overflow for quite a while
		},
		Target: 100e6,
	}

	logger := saetest.NewTBLogger(tb, logging.Warn)
	ctx := logger.CancelOnError(tb.Context())
	snowCtx := snowtest.Context(tb, ids.GenerateTestID())
	snowCtx.Log = logger

	// TODO(arr4n) change this to use [SinceGenesis.Initialize] via the `snow` variable.
	require.NoError(tb, vm.Init(snowCtx, hooks, config, db, &triedb.Config{}, genesis), "Init()")
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
	}
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

// unwrapAndWaitForExecution is equivalent to [blocks.Block.WaitUntilExecuted]
// but accepts a [snowman.Block], which is presumed to contain a [blocks.Block].
// It accepts a Context instead of using [testing.TB.Context] to allow
// integration with [saetest.TBLogger.CancelOnError]. The unwrapped
// [blocks.Block] is returned for convenience.
func unwrapAndWaitForExecution(ctx context.Context, tb testing.TB, snow snowman.Block) *blocks.Block {
	tb.Helper()
	b := unwrap(tb, snow)
	require.NoErrorf(tb, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	return b
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

	preference := sut.genesis.ID()
	require.NoError(t, sut.SetPreference(ctx, preference), "SetPreference([genesis])")
	proposed, err := sut.BuildBlock(ctx)
	require.NoErrorf(t, err, "BuildBlock()")
	assert.Equal(t, preference, proposed.Parent(), "BuildBlock() builds on preference")
	require.Lenf(t, unwrap(t, proposed).Transactions(), numTxs, "%T.Transactions()", proposed)

	snowB, err := sut.ParseBlock(ctx, proposed.Bytes())
	require.NoError(t, err, "ParseBlock(..., BuildBlock().Bytes())")
	require.NoErrorf(t, snowB.Verify(ctx), "%T.Verify()", snowB)
	require.NoErrorf(t, snowB.Accept(ctx), "%T.Accept()", snowB)

	b := unwrapAndWaitForExecution(ctx, t, snowB)
	require.Lenf(t, b.Receipts(), numTxs, "%T.Receipts()", b)
	for i, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Receipts()[%d].Status", i, b)
	}

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

func TestSemanticBlockChecks(t *testing.T) {
	const now = 1e6
	ctx, sut := newSUT(t, 1, blockstest.WithTimestamp(now))
	sut.rawVM.config.Now = func() time.Time {
		return time.Unix(now, 0)
	}

	lastAcceptedID, err := sut.LastAccepted(ctx)
	require.NoError(t, err, "LastAccepted()")
	lastAccepted, err := sut.GetBlock(ctx, lastAcceptedID)
	require.NoError(t, err, "GetBlock(lastAcceptedID)")

	tests := []struct {
		name           string
		parentHash     common.Hash
		acceptedHeight bool
		time           uint64
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
			wantErr:        errFinalizedBlock,
		},
		{
			name:    "block_time_under_minimum",
			time:    1,
			wantErr: errBlockTimeUnderMinimum,
		},
		{
			name:    "block_time_before_parent",
			time:    sut.genesis.BuildTime() - 1,
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
				tt.parentHash = common.Hash(lastAcceptedID)
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
				nil,
				nil,
				tt.receipts,
				saetest.TrieHasher(),
			)
			b := blockstest.NewBlock(
				t,
				ethB,
				nil,
				nil,
			)
			snowB, err := sut.ParseBlock(ctx, b.Bytes())
			require.NoErrorf(t, err, "ParseBlock(...)")
			require.ErrorIs(t, snowB.Verify(ctx), tt.wantErr, "Verify()")
		})
	}
}
