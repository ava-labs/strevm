// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ava-labs/avalanchego/cache/lru"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/libevm/rpc"
	"golang.org/x/exp/slices"
)

var (
	errInvalidPercentile     = errors.New("invalid reward percentile")
	errRequestBeyondHead     = errors.New("request beyond head block")
	errBeyondHistoricalLimit = errors.New("request beyond historical limit")
)

const (
	maxRewardQueryLimit = 100
)

// Backend includes all necessary background APIs for estimator.
type Backend interface {
	ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	SubscribeChainAcceptedEvent(ch chan<- *types.Block) event.Subscription
}

// Estimator recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
type Estimator struct {
	backend Backend
	// cfg holds all estimator parameters set through options.
	cfg config

	lastLock   sync.RWMutex
	lastNumber uint64
	lastPrice  *big.Int

	// clock to decide what set of rules to use when recommending a gas price
	clock mockable.Clock

	closeCh chan struct{}
	// historyCache is a cache of feeInfo for the last [FeeHistoryCacheSize] blocks.
	// We maintain this cache separately from the feeInfoProvider because the FeeHistory
	// can evict recent blocks from the feeInfoProvider's cache.
	historyCache    *lru.Cache[uint64, *feeInfo]
	feeInfoProvider *feeInfoProvider
}

// NewEstimator returns a new gasprice estimator which can recommend suitable
// gasprice for newly created transaction.
func NewEstimator(backend Backend, opts ...EstimatorOption) (*Estimator, error) {
	config := defaultConfig()
	options.ApplyTo(&config, opts...)

	cache := lru.NewCache[uint64, *feeInfo](FeeHistoryCacheSize)
	closeCh := make(chan struct{})
	feeInfoProvider, err := newFeeInfoProvider(backend, config.BlocksCount, closeCh)
	if err != nil {
		return nil, err
	}
	return &Estimator{
		backend:         backend,
		lastPrice:       config.MinPrice,
		cfg:             config,
		closeCh:         closeCh,
		historyCache:    cache,
		feeInfoProvider: feeInfoProvider,
	}, nil
}

// Close stops the estimator's background goroutines.
func (e *Estimator) Close() {
	close(e.closeCh)
}

// SuggestTipCap returns a tip cap so that newly created transaction can have a
// very high chance to be included in the following blocks.
//
// Note, for legacy transactions and the legacy eth_gasPrice RPC call, it will be
// necessary to add the basefee to the returned number to fall back to the legacy
// behavior.
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
		latestBlockNumber     = headNumber
		lowerBlockNumberLimit = uint64(0)
		currentTime           = e.clock.Unix()
		tipResults            []txGasAndReward
	)

	if e.cfg.BlocksCount <= latestBlockNumber {
		lowerBlockNumberLimit = latestBlockNumber - e.cfg.BlocksCount
	}

	// Process block headers in the range calculated for this gas price estimation.
	for i := latestBlockNumber; i > lowerBlockNumberLimit; i-- {
		fi, err := e.feeInfoProvider.getFeeInfo(ctx, i)
		if err != nil {
			return new(big.Int).Set(lastPrice), err
		}

		if fi.timestamp+e.cfg.MaxLookbackSeconds < currentTime {
			break
		}

		tipResults = append(tipResults, fi.txs...)
	}

	price := lastPrice
	lenTipResults := uint64(len(tipResults))
	if lenTipResults > 0 {
		// Although txs are sorted per-block in each feeInfo, we need to
		// re-sort across all recent blocks to find the global percentile.
		slices.SortFunc(tipResults, func(a, b txGasAndReward) int { return a.reward.Cmp(b.reward) })
		price = tipResults[(lenTipResults-1)*(e.cfg.Percentile)/100].reward
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
		return common.Big0, nil, nil, nil, nil // returning with no data and no error means there are no retrievable blocks
	}
	if len(rewardPercentiles) > maxRewardQueryLimit {
		return common.Big0, nil, nil, nil, fmt.Errorf("%w: over the query limit %d", errInvalidPercentile, maxRewardQueryLimit)
	}
	if blocks > e.cfg.MaxCallBlockHistory {
		log.Warn("Sanitizing fee history length", "requested", blocks, "truncated", e.cfg.MaxCallBlockHistory)
		blocks = e.cfg.MaxCallBlockHistory
	}
	for i, p := range rewardPercentiles {
		if p < 0 || p > 100 {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: %f", errInvalidPercentile, p)
		}
		if i > 0 && p <= rewardPercentiles[i-1] {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: #%d:%f >= #%d:%f", errInvalidPercentile, i-1, rewardPercentiles[i-1], i, p)
		}
	}
	lastBlock, blocks, err := e.resolveBlockRange(ctx, unresolvedLastBlock, blocks)
	if err != nil || blocks == 0 {
		return common.Big0, nil, nil, nil, err
	}
	oldestBlock := lastBlock + 1 - blocks

	var (
		reward       = make([][]*big.Int, blocks)
		baseFee      = make([]*big.Int, blocks)
		gasUsedRatio = make([]float64, blocks)
	)

	for blockNumber := oldestBlock; blockNumber < oldestBlock+blocks; blockNumber++ {
		// Check if the context has errored
		if err := ctx.Err(); err != nil {
			return common.Big0, nil, nil, nil, err
		}

		i := blockNumber - oldestBlock
		fi, err := e.getFeeHistoryInfo(ctx, blockNumber)
		if err != nil {
			return common.Big0, nil, nil, nil, err
		}
		baseFee[i] = fi.baseFee
		gasUsedRatio[i] = float64(fi.gasUsed) / float64(fi.gasLimit)
		if len(rewardPercentiles) != 0 {
			reward[i] = fi.processPercentiles(rewardPercentiles)
		}
	}

	// Return nil if no reward percentiles were requested, otherwise it would return a slice.
	if len(rewardPercentiles) == 0 {
		reward = nil
	}
	return new(big.Int).SetUint64(oldestBlock), reward, baseFee, gasUsedRatio, nil
}

// getFeeHistoryInfo gets the feeInfo for a given block number from the history cache.
// If it is not in the cache, it also checks the feeInfoProvider for the block.
// It adds the entry to the history cache if missing.
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
	feeInfo = processBlock(block)
	// We only add the feeInfo to the history cache if we could not find it in the feeInfoProvider's cache.
	// because the FeeHistory can evict recent blocks from the history cache.
	e.historyCache.Put(number, feeInfo)
	return feeInfo, nil
}

// resolveBlockRange resolves the specified block range to absolute block numbers while also
// enforcing backend specific limitations.
// Note: an error is only returned if retrieving the head header has failed. If there are no
// retrievable blocks in the specified range then zero block count is returned with no error.
func (e *Estimator) resolveBlockRange(ctx context.Context, lastBlock rpc.BlockNumber, blocks uint64) (uint64, uint64, error) {
	if blocks == 0 {
		return 0, 0, nil
	}

	lastBlockNumber, err := e.backend.ResolveBlockNumber(lastBlock)
	if err != nil {
		return 0, 0, err
	}

	headNumber, err := e.backend.ResolveBlockNumber(rpc.PendingBlockNumber)
	if err != nil {
		return 0, 0, err
	}
	maxQueryDepth := e.cfg.MaxBlockHistory - 1
	// If the requested last block reaches further back than [config.MaxBlockHistory]
	// from the last accepted block, return an error.
	// Note: this allows some blocks past this point to be fetched since it will
	// start fetching [blocks] from this point.
	if headNumber > maxQueryDepth && lastBlockNumber < headNumber-maxQueryDepth {
		return 0, 0, fmt.Errorf("%w: requested %d, head number %d", errBeyondHistoricalLimit, lastBlock, headNumber)
	}
	// Ensure not trying to retrieve before genesis
	if blocks > lastBlockNumber+1 {
		blocks = lastBlockNumber + 1
	}
	// Truncate blocks range if extending past [config.MaxBlockHistory].
	oldestQueriedIndex := lastBlockNumber - blocks + 1
	if queryDepth := headNumber - oldestQueriedIndex; queryDepth > maxQueryDepth {
		overage := queryDepth - maxQueryDepth
		blocks -= overage
	}
	// It is not possible that [blocks] could be <= 0 after
	// truncation as the [lastBlock] requested will at least be fetchable.
	// Otherwise, we would've returned an error earlier.
	return lastBlockNumber, blocks, nil
}
