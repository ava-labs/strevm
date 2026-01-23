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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestHeaderByHash(t *testing.T) {
	ctx, sut := newSUT(t, 1)
	oldBlock := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))

	for i := 0; i < 3; i++ {
		sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
	}
	recentBlock := sut.lastAcceptedBlock(t)

	tests := []struct {
		name       string
		hash       common.Hash
		wantNil    bool
		wantHash   common.Hash
		wantNumber *big.Int
	}{
		{
			name:       "recent block from cache",
			hash:       recentBlock.Hash(),
			wantHash:   recentBlock.Hash(),
			wantNumber: recentBlock.Header().Number,
		},
		{
			name:       "old block from database",
			hash:       oldBlock.Hash(),
			wantHash:   oldBlock.Hash(),
			wantNumber: oldBlock.Header().Number,
		},
		{
			name:    "non-existent block returns nil",
			hash:    common.HexToHash("0x686920617272616e20616e64207374657068656e2120"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *types.Header
			err := sut.CallContext(ctx, &got, "eth_getHeaderByHash", tt.hash)
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				assert.NotNil(t, got)
				assert.Equal(t, tt.wantHash, got.Hash())
				assert.Equal(t, tt.wantNumber, got.Number)
			}
		})
	}
}

func TestBlockByHash(t *testing.T) {
	ctx, sut := newSUT(t, 1)
	oldBlock := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))

	for i := 0; i < 3; i++ {
		sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
	}
	recentBlock := sut.lastAcceptedBlock(t)

	tests := []struct {
		name       string
		hash       common.Hash
		wantNil    bool
		wantHash   common.Hash
		wantNumber *big.Int
	}{
		{
			name:       "recent block from cache",
			hash:       recentBlock.Hash(),
			wantHash:   recentBlock.Hash(),
			wantNumber: recentBlock.Header().Number,
		},
		{
			name:       "old block from database",
			hash:       oldBlock.Hash(),
			wantHash:   oldBlock.Hash(),
			wantNumber: oldBlock.Header().Number,
		},
		{
			name:    "non-existent block returns nil",
			hash:    common.HexToHash("0x686920617272616e20616e64207374657068656e2120"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]interface{}
			err := sut.CallContext(ctx, &got, "eth_getBlockByHash", tt.hash, false)
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				gotHash, ok := got["hash"].(string)
				require.True(t, ok, "hash field should be a string")
				assert.Equal(t, tt.wantHash, common.HexToHash(gotHash))
				gotNumber, ok := got["number"].(string)
				require.True(t, ok, "number field should be a string")
				wantNumberHex := hexutil.EncodeBig(tt.wantNumber)
				assert.Equal(t, wantNumberHex, gotNumber)
			}
		})
	}
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
