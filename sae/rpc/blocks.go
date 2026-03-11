package rpc

import (
	"context"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
)

func (b *apiBackend) CurrentBlock() *types.Header {
	return b.CurrentHeader()
}

func (b *apiBackend) HeaderByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Header, error) {
	return readByNumber(b.vm, n, rawdb.ReadHeader)
}

func (b *apiBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(b.vm, n, rawdb.ReadBlock)
}

func (b *apiBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return readByHash(b.vm, hash, (*blocks.Block).Header, rawdb.ReadHeader)
}

func (b *apiBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return readByHash(b.vm, hash, (*blocks.Block).EthBlock, rawdb.ReadBlock)
}

func (b *apiBackend) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	return readByNumberOrHash(b, blockNrOrHash, (*blocks.Block).Header, neverErrs(rawdb.ReadHeader))
}

func (b *apiBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	return readByNumberOrHash(b, blockNrOrHash, (*blocks.Block).EthBlock, neverErrs(rawdb.ReadBlock))
}

func (b *apiBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	return readByNumberAndHash(b, hash, number, (*blocks.Block).Body, rawdb.ReadBody)
}
