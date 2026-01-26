// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	opt, vmTime := withVMTime()
	vmTime.Advance(saeparams.Tau)

	ctx, sut := newSUT(t, 1, opt)
	genesis := sut.lastAcceptedBlock(t)

	recipient := common.Address{1, 2, 3, 4}
	transfer := uint256.NewInt(42)
	createTx := func(t *testing.T) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &recipient,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
			Value:     transfer.ToBig(),
		})
	}

	onDisk := sut.createAndAcceptBlock(t, createTx(t)) // a block will be settled after this one
	settledBlock := sut.createAndAcceptBlock(t, createTx(t))
	require.NoErrorf(t, settledBlock.WaitUntilExecuted(ctx), "%T.WaitUntilSettled()", settledBlock)
	vmTime.Set(settledBlock.ExecutedByGasTime().AsTime().Add(saeparams.Tau)) // ensure it will be finalized
	executedBlock := sut.createAndAcceptBlock(t, createTx(t))
	require.NoErrorf(t, executedBlock.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executedBlock)

	cmpOpts := cmp.Options{
		cmputils.EthBlocks(),
		cmputils.Headers(),
		cmpopts.EquateEmpty(),
	}
	//nolint:thelper // not a test helper
	ethclientTests := func(t *testing.T, wantBlock *types.Block) {
		t.Run("BlockByHash", func(t *testing.T) {
			gotBlock, err := sut.BlockByHash(ctx, wantBlock.Hash())
			require.NoErrorf(t, err, "BlockByHash(ctx, %s)", wantBlock.Hash())
			if diff := cmp.Diff(wantBlock, gotBlock, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		t.Run("BlockByNumber", func(t *testing.T) {
			gotBlock, err := sut.BlockByNumber(ctx, new(big.Int).SetUint64(wantBlock.NumberU64()))
			require.NoErrorf(t, err, "BlockByNumber(ctx, %d)", wantBlock.NumberU64())
			if diff := cmp.Diff(wantBlock, gotBlock, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		t.Run("HeaderByHash", func(t *testing.T) {
			gotHeader, err := sut.HeaderByHash(ctx, wantBlock.Hash())
			require.NoErrorf(t, err, "HeaderByHash(ctx, %s)", wantBlock.Hash())
			if diff := cmp.Diff(wantBlock.Header(), gotHeader, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		t.Run("HeaderByNumber", func(t *testing.T) {
			gotHeader, err := sut.HeaderByNumber(ctx, new(big.Int).SetUint64(wantBlock.NumberU64()))
			require.NoErrorf(t, err, "HeaderByNumber(ctx, %d)", wantBlock.NumberU64())
			if diff := cmp.Diff(wantBlock.Header(), gotHeader, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		t.Run("GetTransaction", func(t *testing.T) {
			for _, wantTx := range wantBlock.Transactions() {
				gotTx, isPending, err := sut.TransactionByHash(ctx, wantTx.Hash())
				require.NoErrorf(t, err, "GetTransaction(ctx, %s)", wantTx.Hash())
				require.Falsef(t, isPending, "GetTransaction(...): isPending for %s", wantTx.Hash())
				if diff := cmp.Diff(wantTx, gotTx, cmputils.TransactionsByHash()); diff != "" {
					t.Errorf("Diff (-want +got) for %s:\n%s", wantTx.Hash(), diff)
				}
			}
		})
	}

	t.Run("genesis block", func(t *testing.T) {
		ethclientTests(t, genesis.EthBlock())
	})

	t.Run("on-disk block", func(t *testing.T) {
		ethclientTests(t, onDisk.EthBlock())
	})

	t.Run("settled block", func(t *testing.T) {
		ethclientTests(t, settledBlock.EthBlock())

		t.Run("BlockByNumber Finalized", func(t *testing.T) {
			gotBlock, err := sut.BlockByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
			require.NoErrorf(t, err, "BlockByNumber(ctx, %d)", rpc.FinalizedBlockNumber)
			if diff := cmp.Diff(settledBlock.EthBlock(), gotBlock, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		t.Run("BlockByNumber Safe", func(t *testing.T) {
			gotBlock, err := sut.BlockByNumber(ctx, big.NewInt(int64(rpc.SafeBlockNumber)))
			require.NoErrorf(t, err, "BlockByNumber(ctx, %d)", rpc.SafeBlockNumber)
			if diff := cmp.Diff(settledBlock.EthBlock(), gotBlock, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("executed but not settled block", func(t *testing.T) {
		ethclientTests(t, executedBlock.EthBlock())

		t.Run("BlockByNumber Latest", func(t *testing.T) {
			gotBlock, err := sut.BlockByNumber(ctx, big.NewInt(int64(rpc.LatestBlockNumber)))
			require.NoErrorf(t, err, "BlockByNumber(ctx, %d)", rpc.LatestBlockNumber)
			if diff := cmp.Diff(executedBlock.EthBlock(), gotBlock, cmpOpts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})
	})
}

func testRPCMethod[T any](ctx context.Context, t *testing.T, sut *SUT, method string, want T, args ...any) {
	t.Helper()
	t.Run(method, func(t *testing.T) {
		var got T
		t.Logf("%T.CallContext(ctx, %T, %q, %v...)", sut.rpcClient, got, method, args)
		require.NoError(t, sut.CallContext(ctx, &got, method, args...))
		assert.Equalf(t, want, got, "%T.CallContext(..., %q, ...)", sut.rpcClient, method)
	})
}
