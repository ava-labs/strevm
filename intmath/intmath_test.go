// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package intmath

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

const max = math.MaxUint64

func TestBoundedSubtract(t *testing.T) {
	tests := []struct {
		a, b, floor, want uint64
	}{
		{1, 2, 0, 0}, // a < b
		{2, 1, 0, 1}, // not bounded
		{2, 1, 1, 1}, // a - b == floor
		{2, 2, 1, 1}, // bounded
		{3, 1, 1, 2},
		{max, 10, max - 9, max - 9}, // `a` threshold (`max+1`) would overflow uint64
		{max, 10, max - 11, max - 10},
	}

	for _, tt := range tests {
		if got := BoundedSubtract(tt.a, tt.b, tt.floor); got != tt.want {
			t.Errorf("BoundedSubtract[%T](%[1]d, %d, %d) got %d; want %d", tt.a, tt.b, tt.floor, got, tt.want)
		}
	}
}

func TestMulDiv(t *testing.T) {
	tests := []struct {
		a, b, div, wantQuo, wantRem uint64
	}{
		{
			a: 5, b: 2, div: 3, // 10/3
			wantQuo: 3, wantRem: 1,
		},
		{
			a: 5, b: 3, div: 3, // 15/3
			wantQuo: 5, wantRem: 0,
		},
		{
			a: max, b: 4, div: 8, // must avoid overflow
			wantQuo: max / 2, wantRem: 4,
		},
	}

	for _, tt := range tests {
		if gotQuo, gotRem, err := MulDiv(tt.a, tt.b, tt.div); err != nil || gotQuo != tt.wantQuo || gotRem != tt.wantRem {
			t.Errorf("MulDiv[%T](%[1]d, %d, %d) got (%d, %d, %v); want (%d, %d, nil)", tt.a, tt.b, tt.div, gotQuo, gotRem, err, tt.wantQuo, tt.wantRem)
		}
	}

	if _, _, err := MulDiv[uint64](max, 2, 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("MulDiv[uint64]([max uint64], 2, 1) got error %v; want %v", err, ErrOverflow)
	}
}

func TestCeilDiv(t *testing.T) {
	type test struct {
		num, den, want uint64
	}

	tests := []test{
		{num: 4, den: 2, want: 2},
		{num: 4, den: 1, want: 4},
		{num: 4, den: 3, want: 2},
		{num: 10, den: 3, want: 4},
		{num: max, den: 2, want: 1 << 63}, // must not overflow
	}

	rng := rand.New(rand.NewPCG(0, 0)) //nolint:gosec // Reproducibility is valuable for tests
	for range 50 {
		l := uint64(rng.Uint32())
		r := uint64(rng.Uint32())

		tests = append(tests, []test{
			{num: l*r + 1, den: l, want: r + 1},
			{num: l*r + 0, den: l, want: r},
			{num: l*r - 1, den: l, want: r},
			// l <-> r
			{num: l*r + 1, den: r, want: l + 1},
			{num: l*r + 0, den: r, want: l},
			{num: l*r - 1, den: r, want: l},
		}...)
	}

	for _, tt := range tests {
		if got := CeilDiv(tt.num, tt.den); got != tt.want {
			t.Errorf("CeilDiv[%T](%[1]d, %d) got %d; want %d", tt.num, tt.den, got, tt.want)
		}
	}
}
