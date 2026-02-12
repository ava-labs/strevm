// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gasprice

import (
	"context"
	"math/big"
	"sync"

	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/lru"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/rpc"
	"golang.org/x/exp/slices"
)

// OracleBackend includes all necessary background APIs for oracle.
type OracleBackend interface {
	ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error)
	HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
	SubscribeChainAcceptedEvent(ch chan<- *types.Block) event.Subscription
	LastAcceptedBlock() *types.Block
}

// Oracle recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
type Oracle struct {
	backend   OracleBackend
	lastHead  common.Hash
	lastPrice *big.Int
	// cfg holds all oracle parameters set through options.
	cfg       config
	cacheLock sync.RWMutex
	fetchLock sync.Mutex

	// clock to decide what set of rules to use when recommending a gas price
	clock mockable.Clock

	historyCache    *lru.Cache[uint64, *slimBlock]
	feeInfoProvider *feeInfoProvider
}

// NewOracle returns a new gasprice oracle which can recommend suitable
// gasprice for newly created transaction.
func NewOracle(backend OracleBackend, opts ...OracleOption) (*Oracle, error) {
	config := defaultConfig()
	options.ApplyTo(&config, opts...)

	cache := lru.NewCache[uint64, *slimBlock](FeeHistoryCacheSize)
	headEvent := make(chan core.ChainHeadEvent, 1)
	backend.SubscribeChainHeadEvent(headEvent)
	go func() {
		var lastHead common.Hash
		for ev := range headEvent {
			if ev.Block.ParentHash() != lastHead {
				cache.Purge()
			}
			lastHead = ev.Block.Hash()
		}
	}()
	feeInfoProvider, err := newFeeInfoProvider(backend, config.BlocksCount)
	if err != nil {
		return nil, err
	}
	return &Oracle{
		backend:         backend,
		lastPrice:       config.MinPrice,
		cfg:             config,
		historyCache:    cache,
		feeInfoProvider: feeInfoProvider,
	}, nil
}

// SuggestTipCap returns a tip cap so that newly created transaction can have a
// very high chance to be included in the following blocks.
//
// Note, for legacy transactions and the legacy eth_gasPrice RPC call, it will be
// necessary to add the basefee to the returned number to fall back to the legacy
// behavior.
func (oracle *Oracle) SuggestTipCap(ctx context.Context) (*big.Int, error) {
	head, err := oracle.backend.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if err != nil {
		return nil, err
	}

	headHash := head.Hash()

	// If the latest gasprice is still available, return it.
	oracle.cacheLock.RLock()
	lastHead, lastPrice := oracle.lastHead, oracle.lastPrice
	oracle.cacheLock.RUnlock()
	if headHash == lastHead {
		return new(big.Int).Set(lastPrice), nil
	}
	oracle.fetchLock.Lock()
	defer oracle.fetchLock.Unlock()

	// Try checking the cache again, maybe the last fetch fetched what we need
	oracle.cacheLock.RLock()
	lastHead, lastPrice = oracle.lastHead, oracle.lastPrice
	oracle.cacheLock.RUnlock()
	if headHash == lastHead {
		return new(big.Int).Set(lastPrice), nil
	}
	var (
		latestBlockNumber     = head.Number.Uint64()
		lowerBlockNumberLimit = uint64(0)
		currentTime           = oracle.clock.Unix()
		tipResults            []*big.Int
	)

	if uint64(oracle.cfg.BlocksCount) <= latestBlockNumber {
		lowerBlockNumberLimit = latestBlockNumber - uint64(oracle.cfg.BlocksCount)
	}

	// Process block headers in the range calculated for this gas price estimation.
	for i := latestBlockNumber; i > lowerBlockNumberLimit; i-- {
		feeInfo, err := oracle.getFeeInfo(ctx, i)
		if err != nil {
			return new(big.Int).Set(lastPrice), err
		}

		if feeInfo.timestamp+oracle.cfg.MaxLookbackSeconds < currentTime {
			break
		}

		tipResults = append(tipResults, feeInfo.tips...)
	}

	price := lastPrice
	if len(tipResults) > 0 {
		slices.SortFunc(tipResults, func(a, b *big.Int) int { return a.Cmp(b) })
		price = tipResults[(len(tipResults)-1)*oracle.cfg.Percentile/100]
	}

	if price.Cmp(oracle.cfg.MaxPrice) > 0 {
		price = new(big.Int).Set(oracle.cfg.MaxPrice)
	}
	if price.Cmp(oracle.cfg.MinPrice) < 0 {
		price = new(big.Int).Set(oracle.cfg.MinPrice)
	}
	oracle.cacheLock.Lock()
	oracle.lastHead = headHash
	oracle.lastPrice = price
	oracle.cacheLock.Unlock()

	return new(big.Int).Set(price), nil
}

// getFeeInfo calculates the minimum required tip to be included in a given
// block and returns the value as a feeInfo struct.
func (oracle *Oracle) getFeeInfo(ctx context.Context, number uint64) (*feeInfo, error) {
	feeInfo, ok := oracle.feeInfoProvider.get(number)
	if ok {
		return feeInfo, nil
	}

	// on cache miss, read from database
	block, err := oracle.backend.BlockByNumber(ctx, rpc.BlockNumber(number))
	if err != nil {
		return nil, err
	}
	return oracle.feeInfoProvider.addHeader(ctx, block.Header(), block.Transactions())
}
