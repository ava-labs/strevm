// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"math/big"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks/blockstest"
)

var _ consensus.ChainHeaderReader = (*ReaderAdapter)(nil)

type ReaderAdapter struct {
	chain  *blockstest.ChainBuilder
	db     ethdb.Database
	config *params.ChainConfig
	logger logging.Logger
}

func newReaderAdapter(chain *blockstest.ChainBuilder, db ethdb.Database, cfg *params.ChainConfig, logger logging.Logger) *ReaderAdapter {
	return &ReaderAdapter{
		chain:  chain,
		db:     db,
		config: cfg,
		logger: logger,
	}
}

func (r *ReaderAdapter) Config() *params.ChainConfig {
	return r.config
}

func (r *ReaderAdapter) GetHeader(hash common.Hash, number uint64) *types.Header {
	b, ok := r.chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) CurrentHeader() *types.Header {
	return r.chain.Last().Header()
}

func (r *ReaderAdapter) GetHeaderByHash(hash common.Hash) *types.Header {
	number, ok := r.chain.GetNumberByHash(hash)
	if !ok {
		return nil
	}
	b, ok := r.chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) GetHeaderByNumber(number uint64) *types.Header {
	hash, ok := r.chain.GetHashAtHeight(number)
	if !ok {
		return nil
	}
	b, ok := r.chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) GetTd(hash common.Hash, number uint64) *big.Int {
	td := rawdb.ReadTd(r.db, hash, number)
	if td == nil {
		return nil
	}
	return td
}

func (r *ReaderAdapter) SetTd(hash common.Hash, number uint64, td uint64) {
	rawdb.WriteTd(r.db, hash, number, new(big.Int).SetUint64(td))
}
