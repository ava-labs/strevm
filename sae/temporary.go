// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// This file MUST be deleted before release. It is intended solely to house
// interim identifiers needed for development over multiple PRs.

import (
	"context"
	"errors"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rpc"
)

var errUnimplemented = errors.New("unimplemented")

func (b *ethAPIBackend) ExtRPCEnabled() bool {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) HeaderByNumberOrHash(context.Context, rpc.BlockNumberOrHash) (*types.Header, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) BlockByNumberOrHash(context.Context, rpc.BlockNumberOrHash) (*types.Block, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetReceipts(context.Context, common.Hash) (types.Receipts, error) {
	panic(errUnimplemented)
}

