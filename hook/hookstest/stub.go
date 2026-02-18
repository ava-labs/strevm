// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hookstest provides a test double for SAE's [hook] package.
package hookstest

import (
	"encoding/binary"
	"math/big"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saetest"
)

// Stub implements [hook.Points].
type Stub struct {
	Now            func() time.Time
	Target         gas.Gas
	GasPriceConfig hook.GasPriceConfig
	Ops            []hook.Op
}

var _ hook.Points = (*Stub)(nil)

// BuildHeader constructs a header that builds on top of the parent header. The
// `Extra` field SHOULD NOT be modified as it encodes sub-second block time.
func (s *Stub) BuildHeader(parent *types.Header) *types.Header {
	var now time.Time
	if s.Now != nil {
		now = s.Now()
	} else {
		now = time.Now()
	}

	hdr := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		Time:       uint64(now.Unix()),                                           //nolint:gosec // Known non-negative
		Extra:      binary.BigEndian.AppendUint64(nil, uint64(now.Nanosecond())), //nolint:gosec // Known non-negative
	}
	return hdr
}

// BuildBlock calls [types.NewBlock] with its arguments.
func (*Stub) BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) *types.Block {
	return types.NewBlock(header, txs, nil, receipts, saetest.TrieHasher())
}

// BlockRebuilderFrom returns a block builder that uses the provided block as a
// source of time.
func (s *Stub) BlockRebuilderFrom(b *types.Block) hook.BlockBuilder {
	return &Stub{
		Now: func() time.Time {
			return time.Unix(
				int64(b.Time()), //nolint:gosec // Won't overflow for a few millennia
				s.SubSecondBlockTime(b.Header()).Nanoseconds(),
			)
		},
	}
}

// GasConfigAfter ignores its argument and always returns [Stub.Target] and [Stub.GasPriceConfig].
func (s *Stub) GasConfigAfter(*types.Header) (gas.Gas, hook.GasPriceConfig) {
	return s.Target, s.GasPriceConfig
}

// SubSecondBlockTime returns the sub-second time encoded and stored by
// [Stub.BuildHeader] in the header's `Extra` field. If said field is empty,
// SubSecondBlockTime returns 0.
func (s *Stub) SubSecondBlockTime(hdr *types.Header) time.Duration {
	if len(hdr.Extra) == 0 {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(hdr.Extra)) //nolint:gosec // Test-only code that relies on our own encoding of nanonseconds in [Stub.BuildHeader]
}

// EndOfBlockOps ignores its argument and always returns [Stub.Ops].
func (s *Stub) EndOfBlockOps(*types.Block) []hook.Op {
	return s.Ops
}

// BeforeExecutingBlock is a no-op that always returns nil.
func (*Stub) BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error {
	return nil
}

// AfterExecutingBlock is a no-op.
func (*Stub) AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts) {}
