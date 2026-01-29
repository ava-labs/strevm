// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"math"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/bitutil"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
)

const (
	// bloomServiceThreads is the number of goroutines used globally by an Ethereum
	// instance to service bloombits lookups for all running filters.
	bloomServiceThreads = 16

	// bloomFilterThreads is the number of goroutines used locally per filter to
	// multiplex requests onto the global servicing goroutines.
	bloomFilterThreads = 3

	// bloomRetrievalBatch is the maximum number of bloom bit retrievals to service
	// in a single batch.
	bloomRetrievalBatch = 16

	// bloomRetrievalWait is the maximum time to wait for enough bloom bit requests
	// to accumulate request an entire batch (avoiding hysteresis).
	bloomRetrievalWait = time.Duration(0)

	// bloomThrottling is the time to wait between processing two consecutive index
	// sections. It's useful during chain upgrades to prevent disk overload.
	bloomThrottling = 100 * time.Millisecond
)

type bloomIndexer struct {
	idx *core.ChainIndexer

	size              uint64
	bloomRequests     chan chan *bloombits.Retrieval
	closeBloomHandler chan struct{}
}

// newBloomIndexer returns a chain indexer that generates bloom bits data for the
// canonical chain for fast logs filtering.
func newBloomIndexer(db ethdb.Database, size uint64) *bloomIndexer {
	if size == 0 || size > math.MaxInt32 {
		size = params.BloomBitsBlocks
	}
	backend := &bloomBackend{
		db:   db,
		size: size,
	}
	table := rawdb.NewTable(db, string(rawdb.BloomBitsIndexPrefix))

	idx := &bloomIndexer{
		idx:               core.NewChainIndexer(db, table, backend, size, 0, bloomThrottling, "bloombits"),
		size:              size,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		closeBloomHandler: make(chan struct{}),
	}

	return idx
}

// startBloomHandlers starts a batch of goroutines to accept bloom bit database
// retrievals from possibly a range of filters and serving the data to satisfy.
func (b *bloomIndexer) Start(backend ethapi.Backend) {
	b.idx.Start(backend)

	for i := 0; i < bloomServiceThreads; i++ {
		go func() {
			for {
				select {
				case <-b.closeBloomHandler:
					return

				case request := <-b.bloomRequests:
					task := <-request
					task.Bitsets = make([][]byte, len(task.Sections))
					for i, section := range task.Sections {
						head := rawdb.ReadCanonicalHash(backend.ChainDb(), (section+1)*b.size-1)
						if compVector, err := rawdb.ReadBloomBits(backend.ChainDb(), task.Bit, section, head); err == nil {
							if blob, err := bitutil.DecompressBytes(compVector, int(b.size/8)); err == nil { //nolint:gosec // check performed at constructor
								task.Bitsets[i] = blob
							} else {
								task.Error = err
							}
						} else {
							task.Error = err
						}
					}
					request <- task
				}
			}
		}()
	}
}

func (b *bloomIndexer) Close() error {
	close(b.closeBloomHandler)
	return b.idx.Close()
}

func (b *bloomIndexer) status() (uint64, uint64) {
	sections, _, _ := b.idx.Sections()
	return b.size, sections
}

func (b *bloomIndexer) spawnFilter(ctx context.Context, session *bloombits.MatcherSession) {
	for i := 0; i < bloomFilterThreads; i++ {
		go session.Multiplex(bloomRetrievalBatch, bloomRetrievalWait, b.bloomRequests)
	}
}

var _ core.ChainIndexerBackend = (*bloomBackend)(nil)

// bloomBackend implements a core.ChainIndexer, building up a rotated bloom bits index
// for the Ethereum header bloom filters, permitting blazing fast filtering.
type bloomBackend struct {
	size    uint64               // section size to generate bloombits for
	db      ethdb.Database       // database instance to write index data and metadata into
	gen     *bloombits.Generator // generator to rotate the bloom bits crating the bloom index
	section uint64               // Section is the section number being processed currently
	head    common.Hash          // Head is the hash of the last header processed
}

// Reset implements core.ChainIndexerBackend, starting a new bloombits index
// section.
func (b *bloomBackend) Reset(ctx context.Context, section uint64, lastSectionHead common.Hash) error {
	gen, err := bloombits.NewGenerator(uint(b.size))
	b.gen, b.section, b.head = gen, section, common.Hash{}
	return err
}

// Process implements core.ChainIndexerBackend, adding a new header's bloom into
// the index.
func (b *bloomBackend) Process(ctx context.Context, header *types.Header) error {
	receipts := rawdb.ReadRawReceipts(b.db, header.Hash(), header.Number.Uint64())
	bloom := types.CreateBloom(receipts)
	err := b.gen.AddBloom(uint(header.Number.Uint64()-b.section*b.size), bloom)
	b.head = header.Hash()
	return err
}

// Commit implements core.ChainIndexerBackend, finalizing the bloom section and
// writing it out into the database.
func (b *bloomBackend) Commit() error {
	batch := b.db.NewBatchWithSize((int(b.size) / 8) * types.BloomBitLength) //nolint:gosec // size checked at constructor
	for i := 0; i < types.BloomBitLength; i++ {
		bits, err := b.gen.Bitset(uint(i)) //nolint:gosec // size checked at constructor
		if err != nil {
			return err
		}
		rawdb.WriteBloomBits(batch, uint(i), b.section, b.head, bitutil.CompressBytes(bits))
	}
	return batch.Write()
}

// Prune is not supported for bloom indexes.
func (*bloomBackend) Prune(uint64) error {
	return nil
}
