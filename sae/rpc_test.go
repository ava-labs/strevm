// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
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
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))

	ctx, sut := newSUT(t, 1, opt)
	genesis := sut.lastAcceptedBlock(t)

	createTx := func(t *testing.T) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &common.Address{},
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}

	// Once a block is settled, its ancestors are only accessible from the
	// database.
	onDisk := sut.createAndAcceptBlock(t, createTx(t))

	settled := sut.createAndAcceptBlock(t, createTx(t))
	require.NoErrorf(t, settled.WaitUntilExecuted(ctx), "%T.WaitUntilSettled()", settled)
	vmTime.set(settled.ExecutedByGasTime().AsTime().Add(saeparams.Tau)) // ensure it will be settled

	executed := sut.createAndAcceptBlock(t, createTx(t))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	cmpOpts := cmp.Options{
		cmputils.Blocks(),
		cmputils.Headers(),
		cmpopts.EquateEmpty(),
	}

	for _, b := range []*blocks.Block{genesis, onDisk, settled, executed} {
		t.Run(fmt.Sprintf("block_num_%d", b.Height()), func(t *testing.T) {
			ethB := b.EthBlock()

			testRPCGetter(ctx, t, "BlockByHash", sut.BlockByHash, ethB.Hash(), ethB, cmpOpts...)
			testRPCGetter(ctx, t, "BlockByNumber", sut.BlockByNumber, ethB.Number(), ethB, cmpOpts...)
			testRPCGetter(ctx, t, "HeaderByHash", sut.HeaderByHash, ethB.Hash(), ethB.Header(), cmpOpts...)
			testRPCGetter(ctx, t, "HeaderByNumber", sut.HeaderByNumber, ethB.Number(), ethB.Header(), cmpOpts...)

			t.Run("TransactionByHash", func(t *testing.T) {
				for _, want := range ethB.Transactions() {
					t.Run(want.Hash().String(), func(t *testing.T) {
						got, isPending, err := sut.TransactionByHash(ctx, want.Hash())
						require.NoError(t, err)
						assert.False(t, isPending, "pending")
						if diff := cmp.Diff(want, got, cmputils.TransactionsByHash()); diff != "" {
							t.Errorf("Diff (-want +got):\n%s", diff)
						}
					})
				}
			})
		})
	}

	t.Run("named_blocks", func(t *testing.T) {
		tests := []struct {
			num  rpc.BlockNumber
			want *blocks.Block
		}{
			{rpc.LatestBlockNumber, executed},
			// TODO(arr4n) add a test for pending
			{rpc.SafeBlockNumber, settled},
			{rpc.FinalizedBlockNumber, settled},
		}

		for _, tt := range tests {
			t.Run(tt.num.String(), func(t *testing.T) {
				testRPCGetter(ctx, t, "BlockByNumber", sut.BlockByNumber, big.NewInt(tt.num.Int64()), tt.want.EthBlock(), cmpOpts...)
				testRPCGetter(ctx, t, "HeaderByNumber", sut.HeaderByNumber, big.NewInt(tt.num.Int64()), tt.want.Header(), cmpOpts...)
			})
		}
	})
}

func testRPCGetter[Arg any, T any](ctx context.Context, t *testing.T, funcName string, get func(context.Context, Arg) (T, error), arg Arg, want T, opts ...cmp.Option) {
	t.Helper()
	t.Run(funcName, func(t *testing.T) {
		got, err := get(ctx, arg)
		t.Logf("%s(ctx, %v)", funcName, arg)
		require.NoErrorf(t, err, "%s(%v)", funcName, arg)
		if diff := cmp.Diff(want, got, opts...); diff != "" {
			t.Errorf("%s(%v) diff (-want +got):\n%s", funcName, arg, diff)
		}
	})
}

func testRPCMethod[T any](ctx context.Context, t *testing.T, sut *SUT, method string, want T, args ...any) {
	t.Helper()
	t.Run(method, func(t *testing.T) {
		var got T
		t.Logf("%T.CallContext(ctx, %T, %q, %v...)", sut.rpcClient, got, method, args)
		require.NoError(t, sut.CallContext(ctx, &got, method, args...))
		assert.Equal(t, want, got)
	})
}
