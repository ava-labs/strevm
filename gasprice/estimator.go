// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"

	"github.com/ava-labs/avalanchego/cache/lru"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/rpc"
)

var (
	errBadPercentile         = errors.New("percentile out of range or misordered")
	errFutureBlock           = errors.New("requested block is ahead of accepted head")
	errHistoryDepthExhausted = errors.New("requested block is too far behind accepted head")
)

const (
	// maxPercentilesPerQuery caps the number of reward percentiles a single
	// FeeHistory call may request.
	maxPercentilesPerQuery = 100
)

// Backend abstracts the chain data needed by the Estimator.
type Backend interface {
	ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	SubscribeChainAcceptedEvent(ch chan<- *types.Block) event.Subscription
}

// Estimator provides gas-price suggestions and fee-history data for SAE
// by analysing recently accepted blocks.
type Estimator struct {
	backend Backend
	cfg     Config

	lastLock   sync.RWMutex
	lastNumber uint64
	lastPrice  *big.Int

	// clock to decide what set of rules to use when recommending a gas price
	clock mockable.Clock

	closeCh chan struct{}
	// historyCache is a cache of [feeInfo] for the last [FeeHistoryCacheSize] blocks.
	// We maintain this cache separately from [feeInfoProvider] because
	// [FeeHistory] can evict recent blocks from the provider's bounded cache.
	historyCache    *lru.Cache[uint64, *feeInfo]
	feeInfoProvider *feeInfoProvider
}

// NewEstimator creates an Estimator that serves gas price estimation and fee history.
func NewEstimator(backend Backend, cfg Config) (*Estimator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cache := lru.NewCache[uint64, *feeInfo](FeeHistoryCacheSize)
	closeCh := make(chan struct{})
	feeInfoProvider, err := newFeeInfoProvider(backend, cfg.BlocksCount, closeCh)
	if err != nil {
		return nil, err
	}
	return &Estimator{
		backend:         backend,
		lastPrice:       cfg.MinPrice,
		cfg:             cfg,
		closeCh:         closeCh,
		historyCache:    cache,
		feeInfoProvider: feeInfoProvider,
	}, nil
}

// Close stops the estimator's background goroutines.
func (e *Estimator) Close() {
	close(e.closeCh)
}

// SuggestTipCap recommends a priority-fee (tip) for new transactions by
// sampling effective tips from recently accepted blocks. The result is the
// configured percentile of the combined, tip-sorted transaction set.
//
// For the legacy eth_gasPrice RPC the caller must add the current base fee
// to the returned tip.
func (e *Estimator) SuggestTipCap(ctx context.Context) (*big.Int, error) {
	headNumber, err := e.backend.ResolveBlockNumber(rpc.PendingBlockNumber)
	if err != nil {
		return nil, err
	}

	// Fast path: use RLock so concurrent eth_gasPrice calls can read the
	// cached price in parallel when the head hasn't changed.
	e.lastLock.RLock()
	lastNumber, lastPrice := e.lastNumber, e.lastPrice
	e.lastLock.RUnlock()
	if headNumber == lastNumber {
		return new(big.Int).Set(lastPrice), nil
	}

	// Slow path: acquire exclusive lock to recompute the price.
	// Re-check after acquiring the lock because another goroutine may have
	// already computed the price for this head while we were waiting.
	e.lastLock.Lock()
	defer e.lastLock.Unlock()
	lastNumber, lastPrice = e.lastNumber, e.lastPrice
	if headNumber == lastNumber {
		return new(big.Int).Set(lastPrice), nil
	}

	var (
		newest  = headNumber
		oldest  = uint64(0)
		now     = e.clock.Unix()
		allTips []tipEntry
	)
	if e.cfg.BlocksCount <= newest {
		oldest = newest - e.cfg.BlocksCount
	}

	// Walk backwards through recent blocks, collecting tips until we
	// either exhaust the configured window or hit the lookback age limit.
	// A nil [feeInfo] means the block is unavailable (e.g. pruned); skip it.
	for n := newest; n > oldest; n-- {
		fi, err := e.feeInfoProvider.getFeeInfo(ctx, n)
		if err != nil {
			return new(big.Int).Set(lastPrice), err
		}
		if fi == nil {
			continue
		}
		if fi.timestamp+e.cfg.MaxLookbackSeconds < now {
			break
		}
		allTips = append(allTips, fi.tips...)
	}

	price := lastPrice
	if count := uint64(len(allTips)); count > 0 {
		// Tips are sorted per-block, but we need the global percentile
		// across all recent blocks, so re-sort the combined set.
		slices.SortFunc(allTips, func(a, b tipEntry) int { return a.tip.Cmp(b.tip) })
		price = allTips[(count-1)*e.cfg.Percentile/100].tip
	}

	price = math.BigMax(math.BigMin(price, e.cfg.MaxPrice), e.cfg.MinPrice)

	e.lastNumber = headNumber
	e.lastPrice = price

	return new(big.Int).Set(price), nil
}

// FeeHistory returns data relevant for fee estimation based on the specified range of blocks.
// The range can be specified either with absolute block numbers or ending with the latest
// or pending block. Backends may or may not support gathering data from the pending block
// or blocks older than a certain age (specified in maxHistory). The first block of the
// actually processed range is returned to avoid ambiguity when parts of the requested range
// are not available or when the head has changed during processing this request.
// Three arrays are returned based on the processed blocks:
//   - reward: the requested percentiles of effective priority fees per gas of transactions in each
//     block, sorted in ascending order and weighted by gas limit. (this is different from the libevm implementation which weights by gas used)
//   - baseFee: base fee per gas in the given block
//   - gasUsedRatio: gasUsed/gasLimit in the given block
//
// Note: baseFee includes the next block after the newest of the returned range, because this
// value can be derived from the newest block.
func (e *Estimator) FeeHistory(ctx context.Context, blocks uint64, unresolvedLastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	if blocks < 1 {
		return common.Big0, nil, nil, nil, nil
	}
	if err := validatePercentiles(rewardPercentiles); err != nil {
		return common.Big0, nil, nil, nil, err
	}
	if blocks > e.cfg.MaxCallBlockHistory {
		blocks = e.cfg.MaxCallBlockHistory
	}

	last, blocks, err := e.clampBlockRange(ctx, unresolvedLastBlock, blocks)
	if err != nil || blocks == 0 {
		return common.Big0, nil, nil, nil, err
	}
	first := last + 1 - blocks

	var (
		reward       = make([][]*big.Int, blocks)
		baseFee      = make([]*big.Int, blocks)
		gasUsedRatio = make([]float64, blocks)
		filled       uint64
	)
	for n := first; n <= last; n++ {
		if err := ctx.Err(); err != nil {
			return common.Big0, nil, nil, nil, err
		}
		fi, err := e.getFeeHistoryInfo(ctx, n)
		if err != nil {
			return common.Big0, nil, nil, nil, err
		}
		if fi == nil {
			// Block unavailable (e.g. pruned). Stop and return whatever
			// contiguous data we collected so far.
			break
		}
		baseFee[filled] = fi.baseFee
		gasUsedRatio[filled] = float64(fi.gasUsed) / float64(fi.gasLimit)
		if len(rewardPercentiles) != 0 {
			reward[filled] = fi.rewardPercentiles(rewardPercentiles)
		}
		filled++
	}
	if filled == 0 {
		return common.Big0, nil, nil, nil, nil
	}
	// Truncate to the actually processed range.
	baseFee = baseFee[:filled]
	gasUsedRatio = gasUsedRatio[:filled]
	if len(rewardPercentiles) != 0 {
		reward = reward[:filled]
	} else {
		reward = nil
	}
	return new(big.Int).SetUint64(first), reward, baseFee, gasUsedRatio, nil
}

// validatePercentiles checks that every value in percentiles is in 0..100 and
// strictly ascending. Returns nil when the slice is empty.
func validatePercentiles(percentiles []float64) error {
	if len(percentiles) > maxPercentilesPerQuery {
		return fmt.Errorf("%w: requested %d percentiles, max %d", errBadPercentile, len(percentiles), maxPercentilesPerQuery)
	}
	for i, p := range percentiles {
		if p < 0 || p > 100 {
			return fmt.Errorf("%w: value %f at index %d", errBadPercentile, p, i)
		}
		if i > 0 && p <= percentiles[i-1] {
			return fmt.Errorf("%w: index %d (%f) must be greater than index %d (%f)", errBadPercentile, i, p, i-1, percentiles[i-1])
		}
	}
	return nil
}

// getFeeHistoryInfo returns the [feeInfo] for the given block number,
// consulting (in order) the history LRU cache, the [feeInfoProvider]'s
// recent-block cache, and finally the backend. Results are promoted into
// the history cache on miss.
//
// Returns (nil, nil) when the block is unavailable.
// Callers must handle a nil *[feeInfo] gracefully.
func (e *Estimator) getFeeHistoryInfo(ctx context.Context, number uint64) (*feeInfo, error) {
	if feeInfo, ok := e.historyCache.Get(number); ok {
		return feeInfo, nil
	}
	// if we could not find the feeInfo in the history cache, we check the feeInfoProvider's cache.
	// as it might be found there if it's a recent block.
	// Note: do not fetch the block with feeInfoProvider.getFeeInfo because it can evict recent blocks from the history cache.
	feeInfo, ok := e.feeInfoProvider.cache.Get(number)
	if ok {
		e.historyCache.Put(number, feeInfo)
		return feeInfo, nil
	}
	block, err := e.backend.BlockByNumber(ctx, rpc.BlockNumber(number)) //nolint:gosec // block numbers are always within int64 range
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}
	feeInfo = buildFeeInfo(block)
	// We only add the feeInfo to the history cache if we could not find it in the feeInfoProvider's cache.
	// because the FeeHistory can evict recent blocks from the history cache.
	e.historyCache.Put(number, feeInfo)
	return feeInfo, nil
}

// clampBlockRange turns a caller-supplied (possibly symbolic) block number and
// count into a concrete, accepted-head-relative range. It enforces the
// configured [config.MaxBlockHistory] depth and clamps count so the range
// never extends before genesis.
//
// Returns the resolved last block number and the (possibly reduced) count.
// A zero count with nil error means no retrievable blocks exist in the range.
func (e *Estimator) clampBlockRange(ctx context.Context, reqEnd rpc.BlockNumber, count uint64) (uint64, uint64, error) {
	if count == 0 {
		return 0, 0, nil
	}
	endNum, err := e.backend.ResolveBlockNumber(reqEnd)
	if err != nil {
		return 0, 0, err
	}
	lastAcceptedNumber, err := e.backend.ResolveBlockNumber(rpc.PendingBlockNumber)
	if err != nil {
		return 0, 0, err
	}

	// Reject requests whose *endpoint* is too far behind the accepted head.
	maxDepth := e.cfg.MaxBlockHistory - 1
	if lastAcceptedNumber > maxDepth && endNum < lastAcceptedNumber-maxDepth {
		return 0, 0, fmt.Errorf("%w: block %d requested, accepted head is %d (max depth %d)",
			errHistoryDepthExhausted, reqEnd, lastAcceptedNumber, e.cfg.MaxBlockHistory)
	}

	// Won't reach before genesis.
	if count > endNum+1 {
		count = endNum + 1
	}

	// Trim the start of the range so it stays within MaxBlockHistory of head.
	start := endNum + 1 - count
	if depth := lastAcceptedNumber - start; depth > maxDepth {
		count -= depth - maxDepth
	}
	return endNum, count, nil
}
