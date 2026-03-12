// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpc

import (
	"errors"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
)

func neverErrs[T any](r blocks.DBReader[T]) blocks.DBReaderWithErr[T] {
	return r.WithNilErr()
}

func notFoundIsNil[T any](x *T, err error) (*T, error) {
	if errors.Is(err, blocks.ErrNotFound) {
		return nil, nil
	}
	return x, err
}

func readByNumber[T any](vm Chain, n rpc.BlockNumber, read blocks.DBReader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumber(vm, n, read.WithNilErr()))
}

func readByHash[T any](vm Chain, hash common.Hash, fromMem blocks.Extractor[T], fromDB blocks.DBReader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromHash(vm, hash, fromMem, fromDB.WithNilErr()))
}

func readByNumberOrHash[T any](b *backend, blockNrOrHash rpc.BlockNumberOrHash, fromMem blocks.Extractor[T], fromDB blocks.DBReaderWithErr[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumberOrHash(b, blockNrOrHash, fromMem, fromDB))
}

func readByNumberAndHash[T any](b *backend, h common.Hash, num rpc.BlockNumber, fromMem blocks.Extractor[T], fromDB blocks.DBReader[T]) (*T, error) {
	return notFoundIsNil(blocks.FromNumberAndHash(b, h, num, fromMem, fromDB.WithNilErr()))
}
