// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/libevm/rpc"
)

var (
	errInvalidPercentile     = errors.New("invalid reward percentile")
	errRequestBeyondHead     = errors.New("request beyond head block")
	errBeyondHistoricalLimit = errors.New("request beyond historical limit")
)

const (
	maxQueryLimit = 100
)

// txGasAndReward is sorted in ascending order based on reward
type txGasAndReward struct {
	gasLimit uint64
	reward   *big.Int
}

type slimBlock struct {
	// GasUsed is the sum of all tx gas limits in SAE.
	GasUsed uint64
	// GasLimit is the block gas limit.
	GasLimit uint64
	// BaseFee is the block base fee.
	BaseFee *big.Int
	// Txs is the list of txs in the block.
	Txs []txGasAndReward
}

// processBlock prepares a [slimBlock] from a retrieved block and list of
// receipts. This slimmed block can be cached and used for future calls.
func processBlock(block *types.Block) *slimBlock {
	var sb slimBlock
	if sb.BaseFee = block.BaseFee(); sb.BaseFee == nil {
		sb.BaseFee = new(big.Int)
	}
	sb.GasUsed = block.GasUsed()
	sb.GasLimit = block.GasLimit()
	sorter := make([]txGasAndReward, len(block.Transactions()))
	for i, tx := range block.Transactions() {
		reward, _ := tx.EffectiveGasTip(sb.BaseFee)
		// SAE charges the half of the gas limit per transaction, so tx.Gas() is
		// both the limit and half of the amount charged. block.GasUsed() in the
		// header equals the sum of all tx gas limits.
		sorter[i] = txGasAndReward{gasLimit: tx.Gas(), reward: reward}
	}
	slices.SortStableFunc(sorter, func(a, b txGasAndReward) int {
		return a.reward.Cmp(b.reward)
	})
	sb.Txs = sorter
	return &sb
}

// processPercentiles returns baseFee, gasUsedRatio, and optionally reward percentiles (if any are
// requested)
func (sb *slimBlock) processPercentiles(percentiles []float64) ([]*big.Int, *big.Int, float64) {
	gasUsedRatio := float64(sb.GasUsed) / float64(sb.GasLimit)

	txLen := len(sb.Txs)
	reward := make([]*big.Int, len(percentiles))
	if txLen == 0 {
		// return an all zero row if there are no transactions to gather data from
		for i := range reward {
			reward[i] = new(big.Int)
		}
		return reward, sb.BaseFee, gasUsedRatio
	}

	// Transactions are sorted by tip ascending. We walk through them,
	// accumulating gas limits, to find the gas-weighted reward at each
	// percentile. This is consistent because in SAE, sb.GasUsed equals
	// the sum of all tx gas limits.
	var txIndex int
	sumGasLimit := sb.Txs[0].gasLimit
	for i, p := range percentiles {
		threshold := uint64(float64(sb.GasUsed) * p / 100)
		for sumGasLimit < threshold && txIndex < txLen-1 {
			txIndex++
			sumGasLimit += sb.Txs[txIndex].gasLimit
		}
		reward[i] = sb.Txs[txIndex].reward
	}
	return reward, sb.BaseFee, gasUsedRatio
}

// resolveBlockRange resolves the specified block range to absolute block numbers while also
// enforcing backend specific limitations.
// Note: an error is only returned if retrieving the head header has failed. If there are no
// retrievable blocks in the specified range then zero block count is returned with no error.
func (oracle *Oracle) resolveBlockRange(ctx context.Context, lastBlock rpc.BlockNumber, blocks uint64) (uint64, uint64, error) {
	if blocks == 0 {
		return 0, 0, nil
	}

	lastBlockNumber, err := oracle.backend.ResolveBlockNumber(lastBlock)
	if err != nil {
		return 0, 0, err
	}

	head, err := oracle.backend.HeaderByNumber(ctx, rpc.PendingBlockNumber)
	if err != nil {
		return 0, 0, err
	}
	headNumber := head.Number.Uint64()
	maxQueryDepth := oracle.cfg.MaxBlockHistory - 1
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
		overage := uint64(queryDepth - maxQueryDepth)
		blocks -= overage
	}
	// It is not possible that [blocks] could be <= 0 after
	// truncation as the [lastBlock] requested will at least be fetchable.
	// Otherwise, we would've returned an error earlier.
	return lastBlockNumber, blocks, nil
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
func (oracle *Oracle) FeeHistory(ctx context.Context, blocks uint64, unresolvedLastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	if blocks < 1 {
		return common.Big0, nil, nil, nil, nil // returning with no data and no error means there are no retrievable blocks
	}
	if len(rewardPercentiles) > maxQueryLimit {
		return common.Big0, nil, nil, nil, fmt.Errorf("%w: over the query limit %d", errInvalidPercentile, maxQueryLimit)
	}
	if blocks > oracle.cfg.MaxCallBlockHistory {
		log.Warn("Sanitizing fee history length", "requested", blocks, "truncated", oracle.cfg.MaxCallBlockHistory)
		blocks = oracle.cfg.MaxCallBlockHistory
	}
	for i, p := range rewardPercentiles {
		if p < 0 || p > 100 {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: %f", errInvalidPercentile, p)
		}
		if i > 0 && p <= rewardPercentiles[i-1] {
			return common.Big0, nil, nil, nil, fmt.Errorf("%w: #%d:%f >= #%d:%f", errInvalidPercentile, i-1, rewardPercentiles[i-1], i, p)
		}
	}
	lastBlock, blocks, err := oracle.resolveBlockRange(ctx, unresolvedLastBlock, blocks)
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
		var sb *slimBlock
		if sbCache, ok := oracle.historyCache.Get(blockNumber); ok {
			sb = sbCache
		} else {
			block, err := oracle.backend.BlockByNumber(ctx, rpc.BlockNumber(blockNumber))
			if err != nil {
				return common.Big0, nil, nil, nil, err
			}
			sb = processBlock(block)
			oracle.historyCache.Add(blockNumber, sb)
		}
		if len(rewardPercentiles) != 0 {
			reward[i], baseFee[i], gasUsedRatio[i] = sb.processPercentiles(rewardPercentiles)
		}
	}

	// Return nil if no reward percentiles were requested, otherwise it would return a slice.
	if len(rewardPercentiles) == 0 {
		reward = nil
	}
	return new(big.Int).SetUint64(oldestBlock), reward, baseFee, gasUsedRatio, nil
}
