// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/cmputils"
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
			Data:      []byte{}, // non-nil to align with the behavior of a deserialized tx
		})
	}

	const (
		pendingAccount = 0
		queuedAccount  = 1
	)
	pendingTx := makeTx(pendingAccount)

	_ = makeTx(1) // skip the nonce to gap the mempool
	queuedTx := makeTx(queuedAccount)

	sut.mustSendTx(t, pendingTx)
	sut.mustSendTx(t, queuedTx)
	sut.syncMempool(t)

	txToRPC := func(from common.Address, tx *types.Transaction) *ethapi.RPCTransaction {
		v, r, s := tx.RawSignatureValues()
		return &ethapi.RPCTransaction{
			From:      from,
			Gas:       hexutil.Uint64(tx.Gas()),
			GasPrice:  (*hexutil.Big)(tx.GasPrice()),
			GasFeeCap: (*hexutil.Big)(tx.GasFeeCap()),
			GasTipCap: (*hexutil.Big)(tx.GasTipCap()),
			Hash:      tx.Hash(),
			Input:     hexutil.Bytes(tx.Data()),
			Nonce:     hexutil.Uint64(tx.Nonce()),
			To:        tx.To(),
			Value:     (*hexutil.Big)(tx.Value()),
			Type:      hexutil.Uint64(tx.Type()),
			Accesses:  utils.PointerTo(tx.AccessList()),
			ChainID:   (*hexutil.Big)(tx.ChainId()),
			V:         (*hexutil.Big)(v),
			R:         (*hexutil.Big)(r),
			S:         (*hexutil.Big)(s),
			YParity:   utils.PointerTo(hexutil.Uint64(v.Sign())), //nolint:gosec // Won't overflow
		}
	}
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
				"0": txToRPC(addresses[pendingAccount], pendingTx),
			},
		},
		"queued": {
			addresses[queuedAccount].Hex(): {
				"1": txToRPC(addresses[queuedAccount], queuedTx),
			},
		},
	})

	testRPCMethod(ctx, t, sut, "txpool_contentFrom", map[string]map[string]*ethapi.RPCTransaction{
		"pending": {
			"0": txToRPC(addresses[pendingAccount], pendingTx),
		},
		"queued": {},
	}, addresses[pendingAccount])
	testRPCMethod(ctx, t, sut, "txpool_contentFrom", map[string]map[string]*ethapi.RPCTransaction{
		"pending": {},
		"queued": {
			"1": txToRPC(addresses[queuedAccount], queuedTx),
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

func testRPCMethod[T any](ctx context.Context, t *testing.T, sut *SUT, method string, want T, args ...any) {
	t.Helper()
	t.Run(method, func(t *testing.T) {
		var got T
		t.Logf("%T.CallContext(ctx, %T, %q, %v...)", sut.rpcClient, got, method, args)
		require.NoError(t, sut.CallContext(ctx, &got, method, args...))

		opts := cmp.Options{
			cmputils.HexutilBigs(),
		}
		if diff := cmp.Diff(want, got, opts...); diff != "" {
			t.Errorf("Diff (-want +got):\n%s", diff)
		}
	})
}
