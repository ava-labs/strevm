package intmath

import (
	"math"
	"math/rand"
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
			5, 2, 3, // 10/3
			3, 1,
		},
		{
			5, 3, 3, // 15/3
			5, 0,
		},
		{
			max, 4, 8, // must avoid overflow
			max / 2, 4,
		},
	}

	for _, tt := range tests {
		if gotQuo, gotRem := MulDiv(tt.a, tt.b, tt.div); gotQuo != tt.wantQuo || gotRem != tt.wantRem {
			t.Errorf("MulDiv[%T](%[1]d, %d, %d) got (%d, %d); want (%d, %d)", tt.a, tt.b, tt.div, gotQuo, gotRem, tt.wantQuo, tt.wantRem)
		}
	}
}

func TestCeilDiv(t *testing.T) {
	type test struct {
		num, den, want uint64
	}

	tests := []test{
		{4, 2, 2},
		{4, 1, 4},
		{4, 3, 2},
		{10, 3, 4},
		{max, 2, 1 << 63}, // must not overflow
	}

	rng := rand.New(rand.NewSource(0))
	for range 50 {
		l := uint64(rng.Int63n(math.MaxUint32))
		r := uint64(rng.Int63n(math.MaxUint32))

		tests = append(tests, []test{
			{l*r + 1, l, r + 1},
			{l*r + 0, l, r},
			{l*r - 1, l, r},
			// l <-> r
			{l*r + 1, r, l + 1},
			{l*r + 0, r, l},
			{l*r - 1, r, l},
		}...)
	}

	for _, tt := range tests {
		if got := CeilDiv(tt.num, tt.den); got != tt.want {
			t.Errorf("CeilDiv[%T](%[1]d, %d) got %d; want %d", tt.num, tt.den, got, tt.want)
		}
	}
}
