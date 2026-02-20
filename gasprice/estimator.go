// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gasprice provides gas price statistics and suggestions for timely
// transaction inclusion.
package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/intmath"
)

// Backend that the [Estimator] depends on for chain data.
type Backend interface {
	ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
	LastAcceptedBlock() *blocks.Block
}

// Config allows parameterizing an [Estimator].
type Config struct {
	// Log defaults to [logging.NoLog] if nil.
	Log logging.Logger

	// Now returns the current time. If nil, defaults to [time.Now]
	Now func() time.Time

	// MinSuggestedTip is the minimum suggested tip and the default tip if no
	// better estimate can be made.
	MinSuggestedTip *big.Int
	// SuggestedTipPercentile, in the range (0, 1], specifies what percentile of
	// recent tips.
	SuggestedTipPercentile float64
	MaxSuggestedTip        *big.Int

	// SuggestedTipMaxBlocks specifies the maximum number of recent blocks to fetch
	// for [Estimator.SuggestTipCap].
	SuggestedTipMaxBlocks uint64
	// SuggestedTipMaxDuration specifies how long a block is considered recent
	// for [Estimator.SuggestTipCap].
	SuggestedTipMaxDuration time.Duration

	// HistoryMaxBlocksFromTip specifies the furthest lastBlock behind the last
	// accepted block that can be requested by [Estimator.FeeHistory].
	HistoryMaxBlocksFromTip uint64
	// HistoryMaxBlocks specifies the maximum number of blocks that can be
	// fetched in a single call to [Estimator.FeeHistory].
	HistoryMaxBlocks uint64
}

func (c *Config) setDefaults() {
	if c.Log == nil {
		c.Log = logging.NoLog{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MinSuggestedTip == nil {
		c.MinSuggestedTip = big.NewInt(1 * params.Wei)
	}
	if c.SuggestedTipPercentile == 0 {
		c.SuggestedTipPercentile = .4
	}
	if c.MaxSuggestedTip == nil {
		c.MaxSuggestedTip = big.NewInt(150 * params.Wei)
	}
	if c.SuggestedTipMaxBlocks == 0 {
		c.SuggestedTipMaxBlocks = 20
	}
	if c.SuggestedTipMaxDuration == 0 {
		c.SuggestedTipMaxDuration = time.Minute
	}
	if c.HistoryMaxBlocksFromTip == 0 {
		// Default MaxBlockHistory is chosen to be a value larger than the
		// required fee lookback window that MetaMask uses (20k blocks).
		c.HistoryMaxBlocksFromTip = 25_000
	}
	if c.HistoryMaxBlocks == 0 {
		c.HistoryMaxBlocks = 2048
	}
}

// Estimator provides gas-price suggestions and fee-history data for SAE by
// analyzing recently accepted blocks.
type Estimator struct {
	backend Backend
	c       Config

	lastLock   sync.RWMutex
	lastNumber uint64
	lastPrice  *big.Int

	sub        event.Subscription
	blockCache *blockCache
}

// NewEstimator creates an Estimator for gas tips and fee history.
func NewEstimator(backend Backend, c Config) *Estimator {
	c.setDefaults()

	// New blocks are cached in the background to avoid slow responses after
	// long periods of no requests to the estimator. This allows us to avoid
	// parallelizing reads inside individual API calls.
	//
	// TODO: Consider caching upon acceptance rather than execution.
	events := make(chan core.ChainHeadEvent, 1)
	sub := backend.SubscribeChainHeadEvent(events)
	cache := newBlockCache(
		c.Log,
		backend,
		int(max(
			c.SuggestedTipMaxBlocks,
			c.HistoryMaxBlocksFromTip+c.HistoryMaxBlocks,
		)), //nolint:gosec // Overflow would require misconfiguration
	)
	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case e := <-events:
				cache.cacheBlock(e.Block)
			case <-sub.Err():
				return
			}
		}
	}()

	return &Estimator{
		backend:    backend,
		c:          c,
		lastPrice:  c.MinSuggestedTip,
		sub:        sub,
		blockCache: cache,
	}
}

// SuggestTipCap recommends a priority-fee (tip) for new transactions based on
// tips from recently accepted transactions.
func (e *Estimator) SuggestTipCap(ctx context.Context) (tip *big.Int, _ error) {
	defer func() {
		// Tip is modified by callers of this function, so we must ensure that
		// it is copied.
		if tip != nil {
			tip = new(big.Int).Set(tip)
		}
	}()

	headNumber := e.backend.LastAcceptedBlock().NumberU64()

	e.lastLock.RLock()
	lastNumber, lastPrice := e.lastNumber, e.lastPrice
	e.lastLock.RUnlock()
	if headNumber <= lastNumber {
		return lastPrice, nil
	}

	e.lastLock.Lock()
	defer e.lastLock.Unlock()

	lastNumber, lastPrice = e.lastNumber, e.lastPrice
	if headNumber <= lastNumber {
		return lastPrice, nil
	}

	var (
		newest     = headNumber
		tooOld     = intmath.BoundedSubtract(newest, e.c.SuggestedTipMaxBlocks, 0)
		recentUnix = uint64(e.c.Now().Add(-e.c.SuggestedTipMaxDuration).Unix()) //nolint:gosec // Won't overflow for a long time
		tips       []transaction
	)
	for n := newest; n > tooOld; n-- {
		b := e.blockCache.getBlock(ctx, n)
		if b == nil || b.timestamp < recentUnix {
			break
		}
		tips = append(tips, b.txs...)
	}

	price := lastPrice
	if n := len(tips); n > 0 {
		slices.SortFunc(tips, transaction.Compare)

		i := int(float64(n-1) * e.c.SuggestedTipPercentile) // ∈ [0, n)
		price = tips[i].tip
		price = math.BigMax(price, e.c.MinSuggestedTip)
		price = math.BigMin(price, e.c.MaxSuggestedTip)
	}

	e.lastNumber = headNumber
	e.lastPrice = price
	return price, nil
}

var (
	errHistoryDepthExhausted  = errors.New("requested block is too far behind accepted head")
	errMissingBlock           = errors.New("missing block")
	errMissingWorstCaseBounds = errors.New("head block does not have worst-case bounds")
)

// FeeHistory returns data relevant for fee estimation based on the specified
// range of blocks.
//
// The range can be specified either with absolute block numbers or ending with
// the latest or pending block.
//
// This function returns:
//
//   - The first block of the actually processed range.
//   - The tips paid for each percentile of the cumulative gas limits of the
//     transactions in each block.
//   - The baseFee of each block.
//   - The portion that each block was filled.
func (e *Estimator) FeeHistory(
	ctx context.Context,
	blocks uint64,
	unresolvedLastBlock rpc.BlockNumber,
	rewardPercentiles []float64,
) (
	height *big.Int,
	rewards [][]*big.Int,
	baseFees []*big.Int,
	portionFull []float64,
	_ error,
) {
	if err := validatePercentiles(rewardPercentiles); err != nil {
		return common.Big0, nil, nil, nil, err
	}
	last, err := e.backend.ResolveBlockNumber(unresolvedLastBlock)
	if err != nil {
		return common.Big0, nil, nil, nil, err
	}
	headBlock := e.backend.LastAcceptedBlock()
	head := headBlock.NumberU64()
	if minLast := intmath.BoundedSubtract(head, e.c.HistoryMaxBlocksFromTip, 0); last < minLast {
		return common.Big0, nil, nil, nil, fmt.Errorf("%w: block %d requested, accepted head is %d (max depth %d)",
			errHistoryDepthExhausted,
			last,
			head,
			e.c.HistoryMaxBlocksFromTip,
		)
	}
	blocks = min(
		blocks,               // requested value
		e.c.HistoryMaxBlocks, // DoS protection
		last+1,               // Underflow protection for "first" calculation
	)
	if blocks == 0 {
		return common.Big0, nil, nil, nil, nil
	}

	first := last + 1 - blocks
	var reward [][]*big.Int
	if len(rewardPercentiles) != 0 {
		reward = make([][]*big.Int, 0, blocks)
	}
	var (
		baseFee      = make([]*big.Int, 0, blocks+1)
		gasUsedRatio = make([]float64, 0, blocks)
	)
	for n := first; n <= last; n++ {
		if err := ctx.Err(); err != nil {
			return common.Big0, nil, nil, nil, err
		}
		b := e.blockCache.getBlock(ctx, n)
		if b == nil {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: %d", errMissingBlock, n)
		}

		if len(rewardPercentiles) != 0 {
			reward = append(reward, b.tipPercentiles(rewardPercentiles))
		}
		baseFee = append(baseFee, b.baseFee)
		gasUsedRatio = append(gasUsedRatio, float64(b.gasUsed)/float64(b.gasLimit))
	}
	if last == head {
		bounds := headBlock.WorstCaseBounds()
		if bounds == nil {
			return common.Big0, nil, nil, nil, errMissingWorstCaseBounds
		}
		baseFee = append(baseFee, bounds.NextGasTime.BaseFee().ToBig())
	} else if b := e.blockCache.getBlock(ctx, last+1); b != nil {
		baseFee = append(baseFee, b.baseFee)
	} else {
		return common.Big0, nil, nil, nil, fmt.Errorf("%w: %d", errMissingBlock, last+1)
	}
	return new(big.Int).SetUint64(first), reward, baseFee, gasUsedRatio, nil
}

// Close releases allocated resources.
func (e *Estimator) Close() {
	e.sub.Unsubscribe()
}

const maxPercentiles = 100

var errBadPercentile = errors.New("percentile out of range or misordered")

func validatePercentiles(percentiles []float64) error {
	if len(percentiles) > maxPercentiles {
		return fmt.Errorf("%w: requested %d percentiles, max %d", errBadPercentile, len(percentiles), maxPercentiles)
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
