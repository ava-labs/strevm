// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/cmputils"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest"
)

func TestSubscriptions(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	newHeads := make(chan *types.Header, 1)
	sub, err := sut.SubscribeNewHead(ctx, newHeads)
	require.NoError(t, err, "SubscribeNewHead(...)")
	// The subscription is closed in a defer rather than via t.Cleanup to ensure
	// that is is closed before the rest of the SUT is torn down. Otherwise,
	// there could be a goroutine leak.
	defer sub.Unsubscribe()

	b := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
	got := <-newHeads
	require.Equalf(t, b.Hash(), got.Hash(), "%T.Hash() from %T.SubscribeNewHead(...)", got, sut.Client)
}

func TestWeb3Namespace(t *testing.T) {
	ctx, sut := newSUT(t, 1)
	testRPCMethod(ctx, t, sut, "web3_clientVersion", version.GetVersions().String())
	var (
		preImage hexutil.Bytes = []byte("test")
		want                   = hexutil.Bytes(crypto.Keccak256(preImage))
	)
	testRPCMethod(ctx, t, sut, "web3_sha3", want, preImage)
}

func TestNetNamespace(t *testing.T) {
	testRPCMethodsWithPeers := func(sut *SUT, wantPeerCount hexutil.Uint) {
		t.Helper()

		ctx := sut.context(t)
		testRPCMethod(ctx, t, sut, "net_listening", true)
		testRPCMethod(ctx, t, sut, "net_peerCount", wantPeerCount)
		testRPCMethod(ctx, t, sut, "net_version", fmt.Sprintf("%d", saetest.ChainConfig().ChainID.Uint64()))
	}

	_, sut := newSUT(t, 1) // No peers
	testRPCMethodsWithPeers(sut, 0)

	const (
		numValidators    = 1
		numNonValidators = 2
	)
	n := newNetworkedSUTs(t, numValidators, numNonValidators)
	for _, sut := range n.validators {
		testRPCMethodsWithPeers(sut, numValidators+numNonValidators-1)
	}
	for _, sut := range n.nonValidators {
		testRPCMethodsWithPeers(sut, numValidators)
	}
}

func TestBlockGetters(t *testing.T) {
	opt, setTime := stubbedTime()
	now := time.Unix(0, 0)
	fastForward := func(by time.Duration) {
		now = now.Add(by)
		setTime(now)
	}
	fastForward(saeparams.Tau)

	ctx, sut := newSUT(t, 1, opt)

	testWaitForEvent := func(t *testing.T) {
		t.Helper()
		ev, err := sut.WaitForEvent(ctx)
		require.NoError(t, err)
		require.Equal(t, snowcommon.PendingTxs, ev)
	}
	recipient := common.Address{1, 2, 3, 4}
	createBlock := func(t *testing.T) *blocks.Block {
		t.Helper()
		// Put a tx in the mempool to ensure blocks aren't empty.
		waitForEvDone := make(chan struct{})
		go func() {
			defer close(waitForEvDone)
			t.Run("WaitForEvent_early_unblocks", testWaitForEvent)
		}()

		transfer := uint256.NewInt(42)
		tx := sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &recipient,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
			Value:     transfer.ToBig(),
		})
		sut.mustSendTx(t, tx)

		select {
		case <-waitForEvDone:
		case <-time.After(time.Second):
			t.Error("WaitForEvent() called before SendTx() did not unblock")
		}
		return sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
	}

	settledBlock := createBlock(t)
	fastForward(saeparams.Tau)
	executedBlock := createBlock(t)
	require.NoError(t, executedBlock.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executedBlock)

	type blockGetterTest struct {
		method string
		want   *types.Header
		args   []any
	}
	testsForBlock := func(b *blocks.Block) []blockGetterTest {
		return []blockGetterTest{
			{
				method: "eth_getBlockByHash", want: b.Header(), args: []any{b.Hash(), true},
			},
			{
				method: "eth_getBlockByNumber", want: b.Header(), args: []any{hexutil.Uint64(b.Height()), true},
			},
			{
				method: "eth_getHeaderByHash", want: b.Header(), args: []any{b.Hash()},
			},
			{
				method: "eth_getHeaderByNumber", want: b.Header(), args: []any{hexutil.Uint64(b.Height())},
			},
		}
	}
	t.Run("settled block", func(t *testing.T) {
		for _, tt := range testsForBlock(settledBlock) {
			testRPCMethod(ctx, t, sut, tt.method, *tt.want, tt.args...)
		}
	})

	t.Run("executed but not settled block", func(t *testing.T) {
		for _, tt := range testsForBlock(executedBlock) {
			testRPCMethod(ctx, t, sut, tt.method, *tt.want, tt.args...)
		}
	})
}

func testRPCMethod[T any](ctx context.Context, t *testing.T, sut *SUT, method string, want T, args ...any) {
	t.Helper()
	t.Run(method, func(t *testing.T) {
		var gotRawJSON json.RawMessage
		t.Logf("%T.CallContext(ctx, %T, %q, %v...)", sut.rpcClient, gotRawJSON, method, args)
		require.NoError(t, sut.CallContext(ctx, &gotRawJSON, method, args...))

		var got T
		require.NoErrorf(t, json.Unmarshal(gotRawJSON, &got), "json.Unmarshal(..., %T)", &got)
		opts := cmp.Options{
			cmputils.Headers(),
		}
		if diff := cmp.Diff(want, got, opts...); diff != "" {
			t.Errorf("Diff (-want +got):\n%s", diff)
		}
	})
}
