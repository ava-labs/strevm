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
	"github.com/ava-labs/libevm/libevm/ethapi"
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

func TestTxPoolNamespace(t *testing.T) {
	ctx, sut := newSUT(t, 2)

	addresses := sut.wallet.Addresses()
	makeTx := func(i int) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, i, &types.DynamicFeeTx{
			To:        &addresses[i],
			Gas:       params.TxGas + uint64(i), //nolint:gosec // Won't overflow
			GasFeeCap: big.NewInt(int64(i + 1)),
			Value:     big.NewInt(int64(i + 10)),
		})
	}

	const (
		pendingAccount = 0
		queuedAccount  = 1
	)
	pendingTx := makeTx(pendingAccount)
	pendingRPCTx := ethapi.NewRPCPendingTransaction(pendingTx, nil, saetest.ChainConfig())

	_ = makeTx(queuedAccount) // skip the nonce to gap the mempool
	queuedTx := makeTx(queuedAccount)
	queuedRPCTx := ethapi.NewRPCPendingTransaction(queuedTx, nil, saetest.ChainConfig())

	sut.mustSendTx(t, pendingTx)
	sut.mustSendTx(t, queuedTx)
	sut.syncMempool(t)

	// TODO: This formatting is copied from libevm, consider exposing it somehow
	// or removing the dependency on the exact format.
	txToSummary := func(tx *types.Transaction) string {
		return fmt.Sprintf("%s: %d wei + %d gas × %d wei",
			tx.To(),
			tx.Value().Uint64(),
			tx.Gas(),
			tx.GasFeeCap().Uint64(),
		)
	}

	testRPCMethod(ctx, t, sut, "txpool_content", map[string]map[string]map[string]*ethapi.RPCTransaction{
		"pending": {
			addresses[pendingAccount].Hex(): {
				"0": pendingRPCTx,
			},
		},
		"queued": {
			addresses[queuedAccount].Hex(): {
				"1": queuedRPCTx,
			},
		},
	})

	testRPCMethod(ctx, t, sut, "txpool_contentFrom", map[string]map[string]*ethapi.RPCTransaction{
		"pending": {
			"0": pendingRPCTx,
		},
		"queued": {},
	}, addresses[pendingAccount])
	testRPCMethod(ctx, t, sut, "txpool_contentFrom", map[string]map[string]*ethapi.RPCTransaction{
		"pending": {},
		"queued": {
			"1": queuedRPCTx,
		},
	}, addresses[queuedAccount])

	testRPCMethod(ctx, t, sut, "txpool_inspect", map[string]map[string]map[string]string{
		"pending": {
			addresses[pendingAccount].Hex(): {
				"0": txToSummary(pendingTx),
			},
		},
		"queued": {
			addresses[queuedAccount].Hex(): {
				"1": txToSummary(queuedTx),
			},
		},
	})

	testRPCMethod(ctx, t, sut, "txpool_status", map[string]hexutil.Uint{
		"pending": 1,
		"queued":  1,
	})
}

func TestEthGetters(t *testing.T) {
	opt, vmTime := withVMTime(time.Unix(saeparams.TauSeconds, 0))

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

	for _, b := range []*blocks.Block{genesis, onDisk, settled, executed} {
		t.Run(fmt.Sprintf("block_num_%d", b.Height()), func(t *testing.T) {
			ethB := b.EthBlock()
			testGetByHash(ctx, t, sut, ethB)
			testGetByNumber(ctx, t, sut, ethB, rpc.BlockNumber(b.Number().Int64()))
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
				testGetByNumber(ctx, t, sut, tt.want.EthBlock(), tt.num)
			})
		}

		testRPCMethod(ctx, t, sut, "eth_blockNumber", hexutil.Uint64(executed.Height()))
	})
}

func testGetByHash(ctx context.Context, t *testing.T, sut *SUT, want *types.Block) {
	t.Helper()

	testRPCGetter(ctx, t, "BlockByHash", sut.BlockByHash, want.Hash(), want)
	testRPCGetter(ctx, t, "HeaderByHash", sut.HeaderByHash, want.Hash(), want.Header())
	testRPCGetter(ctx, t, "TransactionCount", sut.TransactionCount, want.Hash(), uint(len(want.Transactions())))

	for i, wantTx := range want.Transactions() {
		t.Run("TransactionByHash", func(t *testing.T) {
			got, isPending, err := sut.TransactionByHash(ctx, wantTx.Hash())
			require.NoError(t, err)
			assert.False(t, isPending, "pending")
			if diff := cmp.Diff(wantTx, got, cmputils.TransactionsByHash()); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})

		hexInd := hexutil.Uint(i) //nolint:gosec // definitely won't overflow
		marshaled, err := wantTx.MarshalBinary()
		require.NoErrorf(t, err, "%T.MarshalBinary()", wantTx)
		testRPCMethod(ctx, t, sut, "eth_getTransactionByBlockHashAndIndex", wantTx, want.Hash(), hexInd)
		testRPCMethod(ctx, t, sut, "eth_getRawTransactionByBlockHashAndIndex", hexutil.Bytes(marshaled), want.Hash(), hexInd)
	}
}

func testGetByNumber(ctx context.Context, t *testing.T, sut *SUT, block *types.Block, n rpc.BlockNumber) {
	t.Helper()
	number := n.Int64()
	testRPCGetter(ctx, t, "BlockByNumber", sut.BlockByNumber, big.NewInt(number), block)
	testRPCGetter(ctx, t, "HeaderByNumber", sut.HeaderByNumber, big.NewInt(number), block.Header())
	testRPCMethod(ctx, t, sut, "eth_getBlockTransactionCountByNumber", hexutil.Uint(len(block.Transactions())), n)

	for i, wantTx := range block.Transactions() {
		hexIdx := hexutil.Uint(i) //nolint:gosec // definitely won't overflow
		marshaled, err := wantTx.MarshalBinary()
		require.NoErrorf(t, err, "%T.MarshalBinary()", wantTx)
		testRPCMethod(ctx, t, sut, "eth_getTransactionByBlockNumberAndIndex", wantTx, n, hexIdx)
		testRPCMethod(ctx, t, sut, "eth_getRawTransactionByBlockNumberAndIndex", hexutil.Bytes(marshaled), n, hexIdx)
	}
}

func testRPCGetter[Arg any, T any](ctx context.Context, t *testing.T, funcName string, get func(context.Context, Arg) (T, error), arg Arg, want T) {
	t.Helper()
	opts := []cmp.Option{
		cmputils.Blocks(),
		cmputils.Headers(),
		cmpopts.EquateEmpty(),
	}

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
		opts := []cmp.Option{
			cmpopts.EquateEmpty(),
			cmputils.TransactionsByHash(),
			cmputils.HexutilBigs(),
		}
		if diff := cmp.Diff(want, got, opts...); diff != "" {
			t.Errorf("Diff (-want +got):\n%s", diff)
		}
	})
}

func TestUncleCount(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	// Create any block (uncle count is always 0 in SAE)
	block := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))

	testRPCMethod(ctx, t, sut, "eth_getUncleCountByBlockHash", hexutil.Uint(0), block.Hash())
	testRPCMethod(ctx, t, sut, "eth_getUncleCountByBlockNumber", hexutil.Uint(0), hexutil.Uint64(block.Height()))
}
