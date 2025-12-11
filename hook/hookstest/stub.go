// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hookstest provides a test double for SAE's [hook] package.
package hookstest

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/hook"
)

// Stub implements [hook.Points].
type Stub struct {
	Target gas.Gas
}

var _ hook.Points = (*Stub)(nil)

// GasTargetAfter ignores its argument and always returns [Stub.Target].
func (s *Stub) GasTargetAfter(*types.Header) gas.Gas {
	return s.Target
}

// SubSecondBlockTime time ignores its arguments and always returns 0.
func (*Stub) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return 0
}

// BeforeExecutingBlock is a no-op that always returns nil.
func (*Stub) BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error {
	return nil
}

// AfterExecutingBlock is a no-op.
func (*Stub) AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts) {}
