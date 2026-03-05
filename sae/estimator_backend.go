// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"
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
	db           ethdb.Database
	lastAccepted *atomic.Pointer[blocks.Block]
	lastSettled  *atomic.Pointer[blocks.Block]
}

var _ gasprice.Backend = (*estimatorBackend)(nil)

// ResolveBlockNumber resolves the block number for the given block number or hash.
// It supports the following block numbers:
// - PendingBlockNumber: the height of the last accepted (head) block
// - LatestBlockNumber: the height of the last executed block
// - SafeBlockNumber, FinalizedBlockNumber: the height of the last settled block
// - Other block numbers: the height of the block with the given number
// It returns an error if the block number is negative or greater than the height of the head block.
func (e *estimatorBackend) ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := e.lastAccepted.Load().Height()

	switch bn {
	case rpc.PendingBlockNumber:
		return head, nil
	case rpc.LatestBlockNumber:
		return e.exec.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return e.lastSettled.Load().Height(), nil
	}

	if bn < 0 {
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > head {
		return 0, fmt.Errorf("%w: block %d", errFutureBlockNotResolved, n)
	}
	return n, nil
}

func (e *estimatorBackend) BlockByNumber(n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(e, e.db, n, neverErrs(rawdb.ReadBlock))
}

func (e *estimatorBackend) LastAcceptedBlock() *blocks.Block {
	return e.lastAccepted.Load()
}
