// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/saexec/saexectest"
)

var _ consensus.ChainHeaderReader = (*ReaderAdapter)(nil)

type ReaderAdapter struct {
	sut *saexectest.SUT
}

func (r *ReaderAdapter) InitializeReaderAdapter(sut *saexectest.SUT) {
	r.sut = sut
}

func (r *ReaderAdapter) Config() *params.ChainConfig {
	return r.sut.ChainConfig()
}

func (r *ReaderAdapter) GetHeader(hash common.Hash, number uint64) *types.Header {
	b, ok := r.sut.Chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) CurrentHeader() *types.Header {
	return r.sut.Chain.Last().Header()
}

func (r *ReaderAdapter) GetHeaderByHash(hash common.Hash) *types.Header {
	number, ok := r.sut.Chain.GetNumberByHash(hash)
	if !ok {
		return nil
	}
	b, ok := r.sut.Chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) GetHeaderByNumber(number uint64) *types.Header {
	hash, ok := r.sut.Chain.GetHashAtHeight(number)
	if !ok {
		return nil
	}
	b, ok := r.sut.Chain.GetBlock(hash, number)
	if !ok {
		return nil
	}
	return b.Header()
}

func (r *ReaderAdapter) GetTd(hash common.Hash, number uint64) *big.Int {
	td := rawdb.ReadTd(r.sut.DB, hash, number)
	if td == nil {
		return nil
	}
	return td
}

func (r *ReaderAdapter) SetTd(hash common.Hash, number uint64, td uint64) {
	rawdb.WriteTd(r.sut.DB, hash, number, new(big.Int).SetUint64(td))
}
