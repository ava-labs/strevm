// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
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
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
)

type bloomIndexer struct {
	idx *core.ChainIndexer

	db                ethdb.Database
	size              uint64
	bloomRequests     chan chan *bloombits.Retrieval
	closeBloomHandler chan struct{}
}

// newBloomIndexer returns a chain indexer that generates bloom bits data for the
// canonical chain for fast logs filtering.
// It must be started with [bloomIndexer.start] before use, and closed with
// [bloomIndexer.close] when no longer needed to avoid resource leaks.
// If size is zero, the default section size defined by [params.BloomBitsBlocks] is used.
func newBloomIndexer(db ethdb.Database, size uint64) *bloomIndexer {
	if size == 0 || size > math.MaxInt32 {
		size = params.BloomBitsBlocks
	}
	backend := &bloomBackend{
		BloomIndexer: core.NewBloomIndexerBackend(db, size),
		db:           db,
	}
	table := rawdb.NewTable(db, string(rawdb.BloomBitsIndexPrefix))
	return &bloomIndexer{
		idx:               core.NewChainIndexer(db, table, backend, size, 0, core.BloomThrottling, "bloombits"),
		db:                db,
		size:              size,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		closeBloomHandler: make(chan struct{}),
	}
}

// start starts a batch of goroutines to accept bloom bit database
// retrievals from possibly a range of filters and serving the data to satisfy,
// as well as the indexer to begin retrieving chain events.
func (b *bloomIndexer) start(backend ethapi.Backend) {
	b.idx.Start(backend)

	eth.StartBloomHandlers(
		b.db,
		b.bloomRequests,
		b.closeBloomHandler,
		b.size,
	)
}

// close stops the bloom indexer and releases all its resources.
func (b *bloomIndexer) close() error {
	close(b.closeBloomHandler)
	return b.idx.Close()
}

// status returns the size of each bloom section and the number of sections
// indexed so far.
func (b *bloomIndexer) status() (uint64, uint64) {
	sections, _, _ := b.idx.Sections()
	return b.size, sections
}

// spawnFilter starts goroutines to multiplex bloom bit retrieval requests for
// a specific filter onto the global bloom servicing goroutines started by [core.StartBloomHandlers].
func (b *bloomIndexer) spawnFilter(ctx context.Context, session *bloombits.MatcherSession) {
	for i := 0; i < eth.BloomFilterThreads; i++ {
		go session.Multiplex(eth.BloomRetrievalBatch, eth.BloomRetrievalWait, b.bloomRequests)
	}
}

var _ core.ChainIndexerBackend = (*bloomBackend)(nil)

// bloomBackend implements a core.ChainIndexerBackend, building up a rotated bloom bits index
// for the Ethereum header bloom filters, permitting fast filtering.
type bloomBackend struct {
	*core.BloomIndexer
	db ethdb.Database // database instance to write index data and metadata into
}

// Process adds a new header's bloom into the index.
// We must read the bloom from the receipts, rather than the header itself, because
// under SAE the header's bloom corresponds to all transactions settled by this block,
// not the transactions included in this block.
func (b *bloomBackend) Process(ctx context.Context, header *types.Header) error {
	receipts := rawdb.ReadRawReceipts(b.db, header.Hash(), header.Number.Uint64())
	bloom := types.CreateBloom(receipts)
	return b.ProcessBloom(header, bloom)
}
