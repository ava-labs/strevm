// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math/big"
	"net/http/httptest"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
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
	)
}

// SUT is the system under test. Testing SHOULD be performed via the embedded
// types as these most accurately reflect the public API. Any access to the
// other fields SHOULD instead be exposed as methods, such as [SUT.stateAt], to
// avoid over-reliance on internal implementation details.
type SUT struct {
	block.ChainVM
	*ethclient.Client

	vm     *VM
	wallet *saetest.Wallet
}

func newSUT(tb testing.TB, numAccounts uint) (context.Context, *SUT) {
	tb.Helper()

	vm := NewVM()
	snow := adaptor.Convert(&SinceGenesis{VM: vm})
	tb.Cleanup(func() {
		ctx := context.WithoutCancel(tb.Context())
		require.NoError(tb, snow.Shutdown(ctx), "Shutdown()")
	})

	config := params.MergedTestChainConfig
	signer := types.LatestSigner(config)
	wallet := saetest.NewUNSAFEWallet(tb, numAccounts, signer)

	db := rawdb.NewMemoryDatabase()
	genesis := blockstest.NewGenesis(tb, db, config, saetest.MaxAllocFor(wallet.Addresses()...))

	hooks := &hookstest.Stub{
		Target: 100e6,
	}

	logger := saetest.NewTBLogger(tb, logging.Warn)
	ctx := logger.CancelOnError(tb.Context())
	snowCtx := snowtest.Context(tb, ids.GenerateTestID())
	snowCtx.Log = logger
	require.NoError(tb, vm.Init(snowCtx, genesis, config, db, &triedb.Config{}, hooks), "Init()")

	handlers, err := snow.CreateHandlers(ctx)
	require.NoErrorf(tb, err, "%T.CreateHandlers()", snow)
	server := httptest.NewServer(handlers["/"])
	tb.Cleanup(server.Close)
	client, err := ethclient.Dial(server.URL)
	require.NoError(tb, err, "ethclient.Dial(http.NewServer(%T.CreateHandlers()))", snow)
	tb.Cleanup(client.Close)

	return ctx, &SUT{
		ChainVM: snow,
		Client:  client,
		vm:      vm,
		wallet:  wallet,
	}
}

func (s *SUT) mustSendTx(tb testing.TB, tx *types.Transaction) {
	tb.Helper()
	require.NoErrorf(tb, s.Client.SendTransaction(tb.Context(), tx), "%T.SendTransaction([%#x])", s.Client, tx.Hash())
}

func (s *SUT) stateAt(tb testing.TB, root common.Hash) *state.StateDB {
	tb.Helper()
	sdb, err := state.New(root, s.vm.exec.StateCache(), nil)
	require.NoErrorf(tb, err, "state.New(%#x, %T.StateCache())", root, s.vm.exec)
	return sdb
}

// syncMempool is a convenience wrapper for calling [txpool.TxPool.Sync], which
// MUST NOT be done in production.
func (s *SUT) syncMempool(tb testing.TB) {
	tb.Helper()
	var _ *txpool.TxPool // maintain import for [comment] rendering
	p := s.vm.mempool.Pool
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

// unwrapAndWait is equivalent to [blocks.Block.WaitUntilExecuted] but accepts a
// [snowman.Block], which is presumed to contain a [blocks.Block]. It accepts a
// context instead of using [testing.TB.Context] to allow integration with
// [saetest.TBLogger.CancelOnError]. The unwrapped [blocks.Block] is returned
// for convenience.
func unwrapAndWait(ctx context.Context, tb testing.TB, snow snowman.Block) *blocks.Block {
	tb.Helper()
	b := unwrap(tb, snow)
	require.NoErrorf(tb, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	return b
}

func TestIntegration(t *testing.T) {
	ctx, sut := newSUT(t, 1)

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
	sut.syncMempool(t)

	proposed, err := sut.BuildBlock(ctx)
	require.NoErrorf(t, err, "BuildBlock()")
	require.Lenf(t, unwrap(t, proposed).Transactions(), numTxs, "%T.Transactions()", proposed)

	snowB, err := sut.ParseBlock(ctx, proposed.Bytes())
	require.NoError(t, err, "ParseBlock(..., BuildBlock().Bytes())")
	require.NoErrorf(t, snowB.Verify(ctx), "%T.Verify()", snowB)
	require.NoErrorf(t, snowB.Accept(ctx), "%T.Accept()", snowB)

	b := unwrapAndWait(ctx, t, snowB)
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
