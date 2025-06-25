// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package intmath provides special-case integer arithmetic.
package intmath

import (
	"math/bits"

	"golang.org/x/exp/constraints"
)

// BoundedSubtract returns `max(a-b,floor)` without underflow.
func BoundedSubtract[T constraints.Unsigned](a, b, floor T) T {
	// If `floor + b` overflows then it's impossible for `a` to ever be large
	// enough for the subtraction to not be bounded.
	minA := floor + b
	if overflow := minA < b; overflow || a <= minA {
		return floor
	}
	return a - b
}

// MulDiv returns the quotient and remainder of `(a*b)/den` without overflow in
// the event that `a*b>=2^64`.
func MulDiv[T ~uint64](a, b, den T) (quo, rem T) {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, r := bits.Div64(hi, lo, uint64(den))
	return T(q), T(r)
}

// CeilDiv returns `ceil(num/den)`, i.e. the rounded-up quotient.
func CeilDiv[T ~uint64](num, den T) T {
	lo, hi := bits.Add64(uint64(num), uint64(den)-1, 0)
	quo, _ := bits.Div64(hi, lo, uint64(den))
	return T(quo)
}
