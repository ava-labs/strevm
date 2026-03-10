package rpc

import (
	"errors"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/blocks"
)

type (
	CanonicalReader[T any]        func(ethdb.Reader, common.Hash, uint64) *T
	CanonicalReaderWithErr[T any] func(ethdb.Reader, common.Hash, uint64) (*T, error)
	BlockAccessor[T any]          func(*blocks.Block) *T
)

func neverErrs[T any](fn func(ethdb.Reader, common.Hash, uint64) *T) CanonicalReaderWithErr[T] {
	return func(r ethdb.Reader, h common.Hash, n uint64) (*T, error) {
		return fn(r, h, n), nil
	}
}

func readByNumber[T any](vm VM, n rpc.BlockNumber, read CanonicalReaderWithErr[T]) (*T, error) {
	num, err := vm.ResolveBlockNumber(n)
	if errors.Is(err, ErrFutureBlockNotResolved) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return read(vm.DB(), rawdb.ReadCanonicalHash(vm.DB(), num), num)
}

// ReadByHash returns `fromMem(b)` if a block with the specified hash is in the
// VM's memory, otherwise it returns `fromDB()` i.f.f. the block was previously
// accepted. If `fromDB()` is called then the block is guaranteed to exist if
// read with [rawdb] functions.
//
// A hash that is in neither of the VM's memory nor the database will result in
// a return of `(nil, errWhenNotFound)` to allow for usage with the [rawdb]
// pattern of returning `(nil, nil)`.
func ReadByHash[T any](vm VM, hash common.Hash, fromMem BlockAccessor[T], fromDB CanonicalReaderWithErr[T], errWhenNotFound error) (*T, error) {
	if blk, ok := vm.BlockFromMemory(hash); ok {
		return fromMem(blk), nil
	}
	num := rawdb.ReadHeaderNumber(vm.DB(), hash)
	if num == nil {
		return nil, errWhenNotFound
	}
	return fromDB(vm.DB(), hash, *num)
}

func readByNumberOrHash[T any](b *apiBackend, blockNrOrHash rpc.BlockNumberOrHash, fromMem BlockAccessor[T], fromDB CanonicalReaderWithErr[T]) (*T, error) {
	n, hash, err := b.resolveBlockNumberOrHash(blockNrOrHash)
	if err != nil {
		return nil, err
	}
	if blk, ok := b.vm.BlockFromMemory(hash); ok {
		return fromMem(blk), nil
	}
	return fromDB(b.vm.DB(), hash, n)
}
