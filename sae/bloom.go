// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math"

	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/eth"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/params"
)

// bloomIndexer provides the [bloomIndexer.BloomStatus] and
// [bloomIndexer.ServiceFilter] methods of an [ethapi.Backend] implementation.
type bloomIndexer struct {
	indexer  *core.ChainIndexer
	size     uint64
	handlers *eth.BloomHandlers
}

func (vm *VM) newBloomIndexer(chain core.ChainIndexerChain, override filters.BloomOverrider, config RPCConfig) *bloomIndexer {
	size := config.BlocksPerBloomSection
	if size == 0 || size > math.MaxInt32 {
		size = params.BloomBitsBlocks
	}

	backend := &bloomBackend{
		BloomIndexer:   core.NewBloomIndexerBackend(vm.db, size),
		BloomOverrider: override,
	}
	table := rawdb.NewTable(vm.db, string(rawdb.BloomBitsIndexPrefix))

	return &bloomIndexer{
		indexer:  core.NewChainIndexer(vm.db, table, backend, size, 0, core.BloomThrottling, "bloombits"),
		size:     size,
		handlers: eth.StartBloomHandlers(vm.db, size),
	}
}

func (s *bloomIndexer) BloomStatus() (uint64, uint64) {
	sections, _, _ := s.indexer.Sections()
	return s.size, sections
}

func (s *bloomIndexer) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	for range eth.BloomFilterThreads {
		go session.Multiplex(eth.BloomRetrievalBatch, eth.BloomRetrievalWait, s.handlers.Requests)
	}
}

var _ core.ChainIndexerBackend = (*bloomBackend)(nil)

// bloomBackend is a wrapper around a [core.BloomIndexer] that overrides
// Process() to allow for custom bloom-filter generation.
type bloomBackend struct {
	*core.BloomIndexer
	filters.BloomOverrider
}

func (b *bloomBackend) Process(ctx context.Context, hdr *types.Header) error {
	return b.ProcessWithBloomOverride(hdr, b.OverrideHeaderBloom(hdr))
}
