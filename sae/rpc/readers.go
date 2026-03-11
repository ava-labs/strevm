package rpc

import (
	"errors"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/blocks"
)

func neverErrs[T any](r blocks.Reader[T]) blocks.ReaderWithErr[T] {
	return r.WithNilErr()
}

func notFoundIsNil[T any](x *T, err error) (*T, error) {
	if errors.Is(err, blocks.ErrNotFound) {
		return nil, nil
	}
	return x, err
}

func readByNumber[T any](vm VM, n rpc.BlockNumber, read blocks.Reader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumber(vm, n, read.WithNilErr()))
}

func readByHash[T any](vm VM, hash common.Hash, fromMem blocks.Extractor[T], fromDB blocks.Reader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromHash(vm, hash, fromMem, fromDB.WithNilErr()))
}

func readByNumberOrHash[T any](b *apiBackend, blockNrOrHash rpc.BlockNumberOrHash, fromMem blocks.Extractor[T], fromDB blocks.ReaderWithErr[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumberOrHash(b.vm, blockNrOrHash, fromMem, fromDB))
}

func readByNumberAndHash[T any](b *apiBackend, h common.Hash, num rpc.BlockNumber, fromMem blocks.Extractor[T], fromDB blocks.Reader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumberAndHash(b.vm, h, num, fromMem, fromDB.WithNilErr()))
}
