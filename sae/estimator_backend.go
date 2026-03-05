// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"sync/atomic"

	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gasprice"
)

// estimatorBackend implements the subset of [ethapi.Backend] required to back a
// [gasprice.Backend].
type estimatorBackend struct {
	chainIndexer
	*numberResolver
	db           ethdb.Database
	lastAccepted *atomic.Pointer[blocks.Block]
}

var _ gasprice.Backend = (*estimatorBackend)(nil)

func (e *estimatorBackend) BlockByNumber(n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(e, e.db, n, neverErrs(rawdb.ReadBlock))
}

func (e *estimatorBackend) LastAcceptedBlock() *blocks.Block {
	return e.lastAccepted.Load()
}
