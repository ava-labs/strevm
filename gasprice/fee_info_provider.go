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

// tipEntry holds a transaction's gas limit and effective tip, used for
// gas-weighted percentile calculations.
type tipEntry struct {
	gas uint64
	tip *big.Int
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
	// tips holds every transaction's gas limit and effective tip, sorted
	// ascending by tip. Used for gas-weighted reward percentile lookups.
	tips []tipEntry
}

// buildFeeInfo extracts fee-relevant data from [block] and returns a
// cacheable [*feeInfo]. The returned tips slice is sorted ascending by
// effective tip so that percentile lookups are O(n).
func buildFeeInfo(block *types.Block) *feeInfo {
	fi := feeInfo{
		timestamp: block.Time(),
		gasUsed:   block.GasUsed(),
		gasLimit:  block.GasLimit(),
	}
	if fi.baseFee = block.BaseFee(); fi.baseFee == nil {
		fi.baseFee = new(big.Int)
	}
	txs := block.Transactions()
	tips := make([]tipEntry, len(txs))
	for i, tx := range txs {
		tip, _ := tx.EffectiveGasTip(fi.baseFee)
		tips[i] = tipEntry{gas: tx.Gas(), tip: tip}
	}
	slices.SortStableFunc(tips, func(a, b tipEntry) int {
		return a.tip.Cmp(b.tip)
	})
	fi.tips = tips
	return &fi
}

// rewardPercentiles computes the gas-weighted reward at each requested
// percentile. Because SAE charges each transaction its half of the gas limit,
// we accumulate gas limits (not receipt gas-used) when walking the
// tip-sorted transaction list.
func (fi *feeInfo) rewardPercentiles(percentiles []float64) []*big.Int {
	out := make([]*big.Int, len(percentiles))
	if len(fi.tips) == 0 {
		// return an all zero row if there are no transactions to gather data from
		for i := range out {
			out[i] = new(big.Int)
		}
		return out
	}

	var txIndex int
	sumGas := fi.tips[0].gas
	for i, p := range percentiles {
		threshold := uint64(float64(fi.gasUsed) * p / 100)
		for sumGas < threshold && txIndex < len(fi.tips)-1 {
			txIndex++
			sumGas += fi.tips[txIndex].gas
		}
		out[i] = fi.tips[txIndex].tip
	}
	return out
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

// addBlock builds a feeInfo for [block] and stores it in the cache.
func (f *feeInfoProvider) addBlock(block *types.Block) *feeInfo {
	fi := buildFeeInfo(block)
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
