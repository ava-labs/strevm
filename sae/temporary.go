// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// This file MUST be deleted before release. It is intended solely to house
// interim identifiers needed for development over multiple PRs.

import (
	"context"
	"errors"
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/rpc"
)

var errUnimplemented = errors.New("unimplemented")

func (b *ethAPIBackend) SuggestGasTipCap(context.Context) (*big.Int, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) FeeHistory(context.Context, uint64, rpc.BlockNumber, []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetPoolNonce(context.Context, common.Address) (uint64, error) {
	panic(errUnimplemented)
}
