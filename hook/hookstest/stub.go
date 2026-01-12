// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hookstest provides a test double for SAE's [hook] package.
package hookstest

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saetest"
)

// Stub implements [hook.Points].
type Stub struct {
	// Now MAY return time at any gas rate but SHOULD aim to mirror the rate
	// equivalent to `Target` to avoid truncation introduced by scaling, which
	// could result in test failures that are difficult to debug.
	Now    func() *proxytime.Time[gas.Gas]
	Target gas.Gas
	Ops    []hook.Op
	TB     testing.TB
}

var _ hook.Points = (*Stub)(nil)

// BuildHeader constructs a header that builds on top of the parent header. The
// `Extra` field SHOULD NOT be modified as it encodes sub-second block time.
func (s *Stub) BuildHeader(parent *types.Header) *types.Header {
	var now *proxytime.Time[gas.Gas]
	if s.Now != nil {
		now = s.Now()
	} else {
		now = NowFunc(time.Now)()
	}

	hdr := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		Time:       now.Unix(),
	}
	setSubSecondBlockTime(hdr, now.Fraction())
	return hdr
}

// SetSubSecondBlockTime encodes `frac` in the header's `Extra` field. This is
// equivalent to [Stub.BuildHeader] encoding, which SHOULD be used instead.
// Typically the only reason to use this function is if a header requires an
// arbitrary parent hash.
func SetSubSecondBlockTime(tb testing.TB, hdr *types.Header, frac proxytime.FractionalSecond[gas.Gas]) {
	tb.Helper()
	require.Emptyf(tb, hdr.Extra, "Overwriting %T.Extra to store block time proxy", hdr)
	setSubSecondBlockTime(hdr, frac)
}

func setSubSecondBlockTime(hdr *types.Header, frac proxytime.FractionalSecond[gas.Gas]) {
	tm := proxytime.New(hdr.Time, frac.Denominator)
	tm.Tick(frac.Numerator)
	hdr.Extra = tm.MarshalCanoto()
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
		Now: func() *proxytime.Time[gas.Gas] {
			tm := new(proxytime.Time[gas.Gas])
			require.NoErrorf(s.TB, tm.UnmarshalCanoto(b.Header().Extra), "%T.UnmarshalCanoto() in %T.BlockRebuilderFrom()", tm, s)
			return tm
		},
	}
}

// GasTargetAfter ignores its argument and always returns [Stub.Target].
func (s *Stub) GasTargetAfter(*types.Header) gas.Gas {
	return s.Target
}

// SubSecondBlockTime unmarshals the time encoded and stored by
// [Stub.BuildHeader] or [SetSubSecondBlockTime], and returns the numerator of
// [proxytime.FractionalSecond]. If the stored denominator is different to the
// `rate` argument, the returned value is scaled. If the header's `Extra` field
// is empty, SubSecondBlockTime returns 0.
func (s *Stub) SubSecondBlockTime(rate gas.Gas, hdr *types.Header) gas.Gas {
	if len(hdr.Extra) == 0 {
		return 0
	}
	tm := new(proxytime.Time[gas.Gas])
	require.NoErrorf(s.TB, tm.UnmarshalCanoto(hdr.Extra), "%T.UnmarshalCanoto() in %T.SubSecondBlockTime()", tm, s)

	frac := tm.Fraction()
	if frac.Denominator == rate {
		return frac.Numerator
	}
	num, _, _ := intmath.MulDiv(frac.Numerator, rate, frac.Denominator) // Guaranteed to not overflow because frac < 1 by definition
	return num
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

// NowFunc returns a function suitable for use in a [Stub], sourcing the current
// time from `src` at nanosecond resolution.
func NowFunc(src func() time.Time) func() *proxytime.Time[gas.Gas] {
	return func() *proxytime.Time[gas.Gas] {
		return proxytime.Of[gas.Gas](src())
	}
}
