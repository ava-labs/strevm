// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"testing"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestWeb3(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	t.Run("clientVersion", func(t *testing.T) {
		var clientVersion string
		err := sut.CallContext(ctx, &clientVersion, "web3_clientVersion")
		require.NoError(t, err)
		assert.Equal(t, version.Current.String(), clientVersion)
	})

	t.Run("sha3", func(t *testing.T) {
		var (
			input  hexutil.Bytes = []byte("test")
			output hexutil.Bytes
		)
		err := sut.CallContext(ctx, &output, "web3_sha3", input)
		require.NoError(t, err)

		expected := hexutil.Bytes(crypto.Keccak256(input))
		assert.Equal(t, expected, output)
	})
}
