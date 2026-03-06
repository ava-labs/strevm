// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gasprice"
)

type estimatorBackend struct {
	*resolver

	acceptedBlocks *event.FeedOf[*blocks.Block]
	db             ethdb.Database
}

var _ gasprice.Backend = (*estimatorBackend)(nil)

func (e *estimatorBackend) BlockByNumber(n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(e, e.db, n, neverErrs(rawdb.ReadBlock))
}

func (e *estimatorBackend) SubscribeAcceptedBlockEvent(ch chan<- *blocks.Block) event.Subscription {
	return e.acceptedBlocks.Subscribe(ch)
}

func (e *estimatorBackend) LastAcceptedBlock() *blocks.Block {
	return e.lastAccepted.Load()
}
