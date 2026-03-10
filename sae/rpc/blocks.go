package rpc

import (
	"context"
	"errors"
	"fmt"

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
	return readByNumber(b.vm, n, neverErrs(rawdb.ReadHeader))
}

func (b *apiBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(b.vm, n, neverErrs(rawdb.ReadBlock))
}

func (b *apiBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return ReadByHash(b.vm, hash, (*blocks.Block).Header, neverErrs(rawdb.ReadHeader), nil /* errWhenNotFound */)
}

func (b *apiBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return ReadByHash(b.vm, hash, (*blocks.Block).EthBlock, neverErrs(rawdb.ReadBlock), nil /* errWhenNotFound */)
}

func (b *apiBackend) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	return readByNumberOrHash(b, blockNrOrHash, (*blocks.Block).Header, neverErrs(rawdb.ReadHeader))
}

func (b *apiBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	return readByNumberOrHash(b, blockNrOrHash, (*blocks.Block).EthBlock, neverErrs(rawdb.ReadBlock))
}

func (b *apiBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	if hash == (common.Hash{}) {
		return nil, errors.New("empty block hash")
	}
	n, err := b.vm.ResolveBlockNumber(number)
	if err != nil {
		return nil, err
	}

	if block, ok := b.vm.BlockFromMemory(hash); ok {
		if block.NumberU64() != n {
			return nil, fmt.Errorf("found block number %d for hash %#x, expected %d", block.NumberU64(), hash, number)
		}
		return block.EthBlock().Body(), nil
	}

	return rawdb.ReadBody(b.vm.DB(), hash, n), nil
}
