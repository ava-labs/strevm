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
	Target        gas.Gas
	SubSecondTime gas.Gas
}

var _ hook.Points = (*Stub)(nil)

// GasTarget ignores its argument and always returns [Stub.Target].
func (s *Stub) GasTarget(*types.Header) gas.Gas {
	return s.Target
}

// SubSecondBlockTime time ignores its arguments and always returns
// [Stub.SubSecondTime].
func (s *Stub) SubSecondBlockTime(gas.Gas, *types.Header) gas.Gas {
	return s.SubSecondTime
}

// BeforeBlock is a no-op that always returns nil.
func (*Stub) BeforeBlock(params.Rules, *state.StateDB, *types.Block) error {
	return nil
}

// AfterBlock is a no-op.
func (*Stub) AfterBlock(*state.StateDB, *types.Block, types.Receipts) {}
