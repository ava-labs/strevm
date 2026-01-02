// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proxytime

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
)

func TestCmpOpt(t *testing.T) {
	// There is enough logic in [CmpOpt] treatment of rate invariants that it
	// warrants testing the test code.

	defaultOpt := CmpOpt[uint64](CmpRateInvariantsByValue)

	withRateInvariants := func(xs ...*uint64) *Time[uint64] {
		tm := New[uint64](42, 1)
		tm.SetRateInvariants(xs...)
		return tm
	}
	zeroA := new(uint64)
	zeroB := new(uint64)
	one := new(uint64)
	*one = 1

	tests := []struct {
		name   string
		a, b   *Time[uint64]
		opt    cmp.Option
		wantEq bool
	}{
		{
			name:   "same_time_no_invariants",
			a:      New[uint64](42, 1),
			b:      New[uint64](42, 1),
			opt:    defaultOpt,
			wantEq: true,
		},
		{
			name:   "different_rate",
			a:      New[uint64](42, 1),
			b:      New[uint64](42, 2),
			opt:    defaultOpt,
			wantEq: false,
		},
		{
			name:   "different_unix_time",
			a:      New[uint64](42, 1),
			b:      New[uint64](41, 1),
			opt:    defaultOpt,
			wantEq: false,
		},
		{
			name: "different_fractional_second",
			a:    New[uint64](42, 100),
			b: func() *Time[uint64] {
				tm := New[uint64](42, 100)
				tm.Tick(1)
				return tm
			}(),
			opt:    defaultOpt,
			wantEq: false,
		},
		{
			name:   "different_but_ignored_invariants",
			a:      withRateInvariants(zeroA),
			b:      withRateInvariants(one),
			opt:    CmpOpt[uint64](IgnoreRateInvariants),
			wantEq: true,
		},
		{
			name:   "equal_invariants_compared_by_value",
			a:      withRateInvariants(zeroA),
			b:      withRateInvariants(zeroB),
			opt:    CmpOpt[uint64](CmpRateInvariantsByValue),
			wantEq: true,
		},
		{
			name:   "unequal_invariants_compared_by_value",
			a:      withRateInvariants(zeroA),
			b:      withRateInvariants(one),
			opt:    CmpOpt[uint64](CmpRateInvariantsByValue),
			wantEq: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantEq, cmp.Equal(tt.a, tt.b, tt.opt))
		})
	}
}
