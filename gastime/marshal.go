// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"github.com/StephenButtolph/canoto"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/strevm/proxytime"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// A TimeMarshaler can marshal a time to and from canoto. It is of limited use
// by itself and SHOULD only be used via a wrapping [Time].
type TimeMarshaler struct { //nolint:tagliatelle // TODO(arr4n) submit linter bug report
	*proxytime.Time[gas.Gas] `canoto:"pointer,1"`
	target                   gas.Gas `canoto:"uint,2"`
	excess                   gas.Gas `canoto:"uint,3"`

	// The nocopy is important, not only for canoto, but because of the use of
	// pointers in [Time.establishInvariants]. See [Time.Clone].
	canotoData canotoData_TimeMarshaler `canoto:"nocopy"`
}

var _ canoto.Message = (*Time)(nil)

// MakeCanoto creates a new empty value.
func (*Time) MakeCanoto() *Time { return new(Time) }

// UnmarshalCanoto unmarshals the bytes into the [TimeMarshaler] and then
// reestablishes invariants.
func (tm *Time) UnmarshalCanoto(bytes []byte) error {
	r := canoto.Reader{
		B: bytes,
	}
	return tm.UnmarshalCanotoFrom(r)
}

// UnmarshalCanotoFrom populates the [TimeMarshaler] from the reader and then
// reestablishes invariants.
func (tm *Time) UnmarshalCanotoFrom(r canoto.Reader) error {
	if err := tm.TimeMarshaler.UnmarshalCanotoFrom(r); err != nil {
		return err
	}
	tm.establishInvariants()
	return nil
}
