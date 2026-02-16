// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"math/big"
	"slices"

	"github.com/ava-labs/avalanchego/cache/lru"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rpc"
)

// additional slots in the header cache to allow processing queries
// for previous blocks (with full number of blocks desired) if new
// blocks are added concurrently.
const feeCacheExtraSlots = 5

type feeInfoProvider struct {
	cache   *lru.Cache[uint64, *feeInfo]
	backend Backend
}

// txGasAndReward is sorted in ascending order based on reward
type txGasAndReward struct {
	gasLimit uint64
	reward   *big.Int
}

type feeInfo struct {
	// timestamp is the block timestamp.
	timestamp uint64
	// gasUsed is the sum of all tx gas limits in SAE.
	gasUsed uint64
	// gasLimit is the block gas limit.
	gasLimit uint64
	// baseFee is the block base fee.
	baseFee *big.Int
	// txs is the list of txs in the block.
	// txs is sorted in ascending order based on reward.
	txs []txGasAndReward
}

// processBlock prepares a [feeInfo] from a retrieved block and list of
// receipts. txs is sorted in ascending order based on reward.
// This feeInfo can be cached and used for future calls.
func processBlock(block *types.Block) *feeInfo {
	var fi feeInfo
	if fi.baseFee = block.BaseFee(); fi.baseFee == nil {
		fi.baseFee = new(big.Int)
	}
	fi.timestamp = block.Time()
	fi.gasUsed = block.GasUsed()
	fi.gasLimit = block.GasLimit()
	sorter := make([]txGasAndReward, len(block.Transactions()))
	for i, tx := range block.Transactions() {
		reward, _ := tx.EffectiveGasTip(fi.baseFee)
		// SAE charges the half of the gas limit per transaction, so tx.Gas() is
		// both the limit and half of the amount charged. block.GasUsed() in the
		// header equals the sum of all tx gas limits.
		sorter[i] = txGasAndReward{gasLimit: tx.Gas(), reward: reward}
	}
	slices.SortStableFunc(sorter, func(a, b txGasAndReward) int {
		return a.reward.Cmp(b.reward)
	})
	fi.txs = sorter
	return &fi
}

// processPercentiles returns reward percentiles
func (fi *feeInfo) processPercentiles(percentiles []float64) []*big.Int {
	txLen := len(fi.txs)
	reward := make([]*big.Int, len(percentiles))
	if txLen == 0 {
		// return an all zero row if there are no transactions to gather data from
		for i := range reward {
			reward[i] = new(big.Int)
		}
		return reward
	}

	// Transactions are sorted by tip ascending. We walk through them,
	// accumulating gas limits, to find the gas-weighted reward at each
	// percentile. This is consistent because in SAE, fi.GasUsed equals
	// the sum of all tx gas limits.
	var txIndex int
	sumGasLimit := fi.txs[0].gasLimit
	for i, p := range percentiles {
		threshold := uint64(float64(fi.gasUsed) * p / 100)
		for sumGasLimit < threshold && txIndex < txLen-1 {
			txIndex++
			sumGasLimit += fi.txs[txIndex].gasLimit
		}
		reward[i] = fi.txs[txIndex].reward
	}
	return reward
}

// newFeeInfoProvider returns a bounded buffer with [size] slots to
// store [*feeInfo] for the most recently accepted blocks.
// The caller must close [closeCh] to stop the background goroutine.
func newFeeInfoProvider(backend Backend, size uint64, closeCh <-chan struct{}) (*feeInfoProvider, error) {
	fc := &feeInfoProvider{
		backend: backend,
	}
	if size == 0 {
		// if size is zero, we return early as there is no
		// reason for a goroutine to subscribe to the chain's
		// accepted event.
		fc.cache = lru.NewCache[uint64, *feeInfo](int(size))
		return fc, nil
	}

	fc.cache = lru.NewCache[uint64, *feeInfo](int(size + feeCacheExtraSlots)) //nolint:gosec // size is bounded by config
	// subscribe to the chain accepted event
	acceptedEvent := make(chan *types.Block, 1)
	sub := backend.SubscribeChainAcceptedEvent(acceptedEvent)
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case acceptedBlock := <-acceptedEvent:
				fc.addBlock(acceptedBlock)
			case <-closeCh:
				return
			case <-sub.Err():
				return
			}
		}
	}()
	return fc, fc.populateCache(size)
}

// addBlock processes block into a feeInfo and caches the result.
func (f *feeInfoProvider) addBlock(block *types.Block) *feeInfo {
	fi := processBlock(block)
	f.cache.Put(block.NumberU64(), fi)
	return fi
}

// getFeeInfo calculates the minimum required tip to be included in a given
// block and returns the value as a feeInfo struct.
// It adds the entry to the cache if missing.
func (f *feeInfoProvider) getFeeInfo(ctx context.Context, number uint64) (*feeInfo, error) {
	feeInfo, ok := f.cache.Get(number)
	if ok {
		return feeInfo, nil
	}

	// on cache miss, read from database
	block, err := f.backend.BlockByNumber(ctx, rpc.BlockNumber(number)) //nolint:gosec // block numbers are always within int64 range
	if err != nil {
		return nil, err
	}
	return f.addBlock(block), nil
}

// populateCache populates [f] with [size] blocks up to last accepted.
// Note: assumes [size] is greater than zero.
func (f *feeInfoProvider) populateCache(size uint64) error {
	lastAccepted, err := f.backend.ResolveBlockNumber(rpc.PendingBlockNumber)
	if err != nil {
		return err
	}
	lowerBlockNumber := uint64(0)
	if size-1 <= lastAccepted { // Note: "size-1" because we need a total of size blocks.
		lowerBlockNumber = lastAccepted - (size - 1)
	}

	for i := lowerBlockNumber; i <= lastAccepted; i++ {
		block, err := f.backend.BlockByNumber(context.Background(), rpc.BlockNumber(i)) //nolint:gosec // block numbers are always within int64 range
		if err != nil {
			return err
		}
		_ = f.addBlock(block)
	}
	return nil
}
