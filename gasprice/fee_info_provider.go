// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"math/big"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rpc"
	lru "github.com/hashicorp/golang-lru"
)

// additional slots in the header cache to allow processing queries
// for previous blocks (with full number of blocks desired) if new
// blocks are added concurrently.
const feeCacheExtraSlots = 5

type feeInfoProvider struct {
	cache   *lru.Cache
	backend OracleBackend
}

// feeInfo is the type of data stored in feeInfoProvider's cache.
type feeInfo struct {
	tips      []*big.Int // tips for txs to be included in the block
	timestamp uint64     // timestamp of the block header
}

// newFeeInfoProvider returns a bounded buffer with [size] slots to
// store [*feeInfo] for the most recently accepted blocks.
// The caller must close [closeCh] to stop the background goroutine.
func newFeeInfoProvider(backend OracleBackend, size int, closeCh <-chan struct{}) (*feeInfoProvider, error) {
	fc := &feeInfoProvider{
		backend: backend,
	}
	if size == 0 {
		// if size is zero, we return early as there is no
		// reason for a goroutine to subscribe to the chain's
		// accepted event.
		fc.cache, _ = lru.New(size)
		return fc, nil
	}

	fc.cache, _ = lru.New(size + feeCacheExtraSlots)
	// subscribe to the chain accepted event
	acceptedEvent := make(chan *types.Block, 1)
	sub := backend.SubscribeChainAcceptedEvent(acceptedEvent)
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case ev := <-acceptedEvent:
				fc.addHeader(context.Background(), ev.Header(), ev.Transactions())
			case <-closeCh:
				return
			case <-sub.Err():
				return
			}
		}
	}()
	return fc, fc.populateCache(size)
}

// addHeader processes header into a feeInfo struct and caches the result.
func (f *feeInfoProvider) addHeader(ctx context.Context, header *types.Header, txs []*types.Transaction) (*feeInfo, error) {
	tips := make([]*big.Int, 0, len(txs))
	for _, tx := range txs {
		tip, err := tx.EffectiveGasTip(header.BaseFee)
		if err != nil {
			return nil, err
		}
		tips = append(tips, tip)
	}

	feeInfo := &feeInfo{
		timestamp: header.Time,
		tips:      tips,
	}
	f.cache.Add(header.Number.Uint64(), feeInfo)
	return feeInfo, nil
}

// get returns the feeInfo for block with [number] if present in the cache
// and a boolean representing if it was found.
func (f *feeInfoProvider) get(number uint64) (*feeInfo, bool) {
	// Note: use Peek on LRU to use it as a bounded buffer.
	feeInfoIntf, ok := f.cache.Peek(number)
	if ok {
		return feeInfoIntf.(*feeInfo), ok
	}
	return nil, ok
}

// populateCache populates [f] with [size] blocks up to last accepted.
// Note: assumes [size] is greater than zero.
func (f *feeInfoProvider) populateCache(size int) error {
	lastAccepted, err := f.backend.ResolveBlockNumber(rpc.PendingBlockNumber)
	if err != nil {
		return err
	}
	lowerBlockNumber := uint64(0)
	if uint64(size-1) <= lastAccepted { // Note: "size-1" because we need a total of size blocks.
		lowerBlockNumber = lastAccepted - uint64(size-1)
	}

	for i := lowerBlockNumber; i <= lastAccepted; i++ {
		block, err := f.backend.BlockByNumber(context.Background(), rpc.BlockNumber(i))
		if err != nil {
			return err
		}
		_, err = f.addHeader(context.Background(), block.Header(), block.Transactions())
		if err != nil {
			return err
		}
	}
	return nil
}
