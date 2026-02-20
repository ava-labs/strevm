// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/stretchr/testify/require"
)

func TestFeeInfoProvider(t *testing.T) {
	backend := newTestBackend(t, 2, testGenBlock(t, 55, 80))
	closeCh := make(chan struct{})
	defer close(closeCh)
	f, err := newFeeInfoProvider(backend, 2, closeCh)
	require.NoError(t, err)

	// check that accepted event was subscribed
	require.NotNil(t, backend.acceptedCh)

	// check fee infos were cached
	require.Equal(t, 2, f.cache.Len())

	// send a block through the subscription and verify it gets cached
	header := &types.Header{Number: big.NewInt(3), ParentHash: backend.lastBlock().Hash()}
	block := types.NewBlockWithHeader(header)
	backend.acceptedCh <- core.ChainHeadEvent{Block: block}

	require.Eventually(t, func() bool {
		_, ok := f.cache.Get(3)
		return ok
	}, 5*time.Second, 10*time.Millisecond)
}

func TestFeeInfoProviderCacheSize(t *testing.T) {
	size := uint64(5)
	overflow := uint64(3)
	backend := newTestBackend(t, 0, testGenBlock(t, 55, 370))
	closeCh := make(chan struct{})
	defer close(closeCh)
	f, err := newFeeInfoProvider(backend, size, closeCh)
	require.NoError(t, err)

	// add [overflow] more elements than what will fit in the cache
	// to test eviction behavior.
	for i := uint64(0); i < size+feeCacheExtraSlots+overflow; i++ {
		header := &types.Header{Number: new(big.Int).SetUint64(i)}
		block := types.NewBlockWithHeader(header)
		f.addBlock(block)
		require.NoError(t, err)
	}

	// these numbers should be evicted
	for i := uint64(0); i < overflow; i++ {
		feeInfo, ok := f.cache.Get(i)
		require.False(t, ok)
		require.Nil(t, feeInfo)
	}

	// these numbers should be present
	for i := overflow; i < size+feeCacheExtraSlots+overflow; i++ {
		feeInfo, ok := f.cache.Get(i)
		require.True(t, ok)
		require.NotNil(t, feeInfo)
	}
}
