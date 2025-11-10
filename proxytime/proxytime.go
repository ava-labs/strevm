// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package proxytime measures the passage of time based on a proxy unit and
// associated unit rate.
package proxytime

import (
	"cmp"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/ava-labs/strevm/intmath"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// A Duration is a type parameter for use as the unit of passage of [Time].
type Duration interface {
	~uint64
}

// Time represents an instant in time, its passage measured by an arbitrary unit
// of duration. It is not thread safe nor is the zero value valid.
type Time[D Duration] struct {
	seconds uint64 `canoto:"uint,1"`
	// invariant: fraction < hertz
	fraction D `canoto:"uint,2"`
	hertz    D `canoto:"uint,3"`

	rateInvariants []*D

	canotoData canotoData_Time `canoto:"nocopy"`
}

// IMPORTANT: keep [Time.Clone] next to the struct definition to make it easier
// to check that all fields are copied.

// Clone returns a copy of the time. Note that it does NOT copy the pointers
// passed to [Time.SetRateInvariants] as this risks coupling the clone with the
// wrong invariants.
func (tm *Time[D]) Clone() *Time[D] {
	return &Time[D]{
		seconds:  tm.seconds,
		fraction: tm.fraction,
		hertz:    tm.hertz,
	}
}

// New returns a new [Time], set from a Unix timestamp. The passage of `hertz`
// units is equivalent to a tick of 1 second.
func New[D Duration](unixSeconds uint64, hertz D) *Time[D] {
	return &Time[D]{
		seconds: unixSeconds,
		hertz:   hertz,
	}
}

// Unix returns tm as a Unix timestamp.
func (tm *Time[D]) Unix() uint64 {
	return tm.seconds
}

// A FractionalSecond represents a sub-second duration of time. The numerator is
// equivalent to a value passed to [Time.Tick] when [Time.Rate] is the
// denominator.
type FractionalSecond[D Duration] struct {
	Numerator, Denominator D
}

// Fraction returns the fractional-second component of the time, denominated in
// [Time.Rate].
func (tm *Time[D]) Fraction() FractionalSecond[D] {
	return FractionalSecond[D]{tm.fraction, tm.hertz}
}

// Rate returns the proxy duration required for the passage of one second.
func (tm *Time[D]) Rate() D {
	return tm.hertz
}

// Tick advances the time by `d`.
func (tm *Time[D]) Tick(d D) {
	frac, carry := bits.Add64(uint64(tm.fraction), uint64(d), 0)
	quo, rem := bits.Div64(carry, frac, uint64(tm.hertz))
	tm.seconds += quo
	tm.fraction = D(rem)
}

// FastForwardTo sets the time to the specified Unix timestamp if it is in the
// future, returning the integer and fraction number of seconds by which the
// time was advanced. The fraction is always denominated in [Time.Rate].
func (tm *Time[D]) FastForwardTo(to uint64) (uint64, FractionalSecond[D]) {
	if to <= tm.seconds {
		return 0, FractionalSecond[D]{0, tm.hertz}
	}

	sec := to - tm.seconds
	var frac D
	if tm.fraction > 0 {
		frac = tm.hertz - tm.fraction
		sec--
	}

	tm.seconds = to
	tm.fraction = 0

	return sec, FractionalSecond[D]{frac, tm.hertz}
}

// SetRate changes the unit rate at which time passes. The requisite integer
// division may result in rounding down of the fractional-second component of
// time, the amount of which is returned.
//
// If no values have been registered with [Time.SetRateInvariants] then SetRate
// will always return a nil error. A non-nil error will only be returned if any
// of the rate-invariant values overflows a uint64 due to the scaling.
func (tm *Time[D]) SetRate(hertz D) (truncated FractionalSecond[D], err error) {
	frac, truncated, err := tm.scale(tm.fraction, hertz)
	if err != nil {
		// If this happens then there is a bug in the implementation. The
		// invariant that `tm.fraction < tm.hertz` makes overflow impossible as
		// the scaled fraction will be less than the new rate.
		return FractionalSecond[D]{}, fmt.Errorf("fractional-second time: %w", err)
	}

	// Avoid scaling some but not all rate invariants if one results in an
	// error.
	scaled := make([]D, len(tm.rateInvariants))
	for i, v := range tm.rateInvariants {
		scaled[i], _, err = tm.scale(*v, hertz)
		if err != nil {
			return FractionalSecond[D]{}, fmt.Errorf("rate invariant [%d]: %w", i, err)
		}
	}
	for i, v := range tm.rateInvariants {
		*v = scaled[i]
	}

	tm.fraction = frac
	tm.hertz = hertz
	return truncated, nil
}

// SetRateInvariants sets units that, whenever [Time.SetRate] is called, will be
// scaled relative to the change in rate. Scaling may be affected by the same
// truncation described for [Time.SetRate]. Truncation aside, the rational
// numbers formed by the invariants divided by the rate will each remain equal
// despite their change in denominator.
//
// The pointers MUST NOT be nil.
func (tm *Time[D]) SetRateInvariants(inv ...*D) {
	tm.rateInvariants = inv
}

// scale returns `val`, scaled from the existing [Time.Rate] to the newly
// specified one. See [Time.SetRate] for details about truncation and overflow
// errors.
func (tm *Time[D]) scale(val, newRate D) (scaled D, truncated FractionalSecond[D], err error) {
	scaled, trunc, err := intmath.MulDiv(val, newRate, tm.hertz)
	if err != nil {
		return 0, FractionalSecond[D]{}, fmt.Errorf("scaling %d from rate of %d to %d: %w", val, tm.hertz, newRate, err)
	}
	return scaled, FractionalSecond[D]{Numerator: trunc, Denominator: tm.hertz}, nil
}

// Compare returns
//
//	-1 if tm is before u
//	 0 if tm and u represent the same instant
//	+1 if tm is after u.
//
// Results are undefined if [Time.Rate] is different for the two instants.
func (tm *Time[D]) Compare(u *Time[D]) int {
	if c := cmp.Compare(tm.seconds, u.seconds); c != 0 {
		return c
	}
	return cmp.Compare(tm.fraction, u.fraction)
}

// CompareUnix is equivalent to [Time.Compare] against a zero-fractional-second
// instant in time. Note that it does NOT only compare the seconds and that if
// `tm` has the same [Time.Unix] as `sec` but non-zero [Time.Fraction] then
// CompareUnix will return 1.
func (tm *Time[D]) CompareUnix(sec uint64) int {
	return tm.Compare(&Time[D]{seconds: sec})
}

// AsTime converts the proxy time to a standard [time.Time] in UTC. AsTime is
// analogous to setting a rate of 1e9 (nanosecond), which might result in
// truncation. The second-range limitations documented on [time.Unix] also apply
// to AsTime.
func (tm *Time[D]) AsTime() time.Time {
	if tm.seconds > math.MaxInt64 { // keeps gosec linter happy
		return time.Unix(math.MaxInt64, math.MaxInt64)
	}
	// The error can be ignored as the fraction is always less than the rate and
	// therefore the scaled value can never overflow.
	nsec, _ /*remainder*/, _ := tm.scale(tm.fraction, 1e9)
	return time.Unix(int64(tm.seconds), int64(nsec)).In(time.UTC)
}

// String returns the time as a human-readable string. It is not intended for
// parsing and its format MAY change.
func (tm *Time[D]) String() string {
	f := tm.Fraction()
	return fmt.Sprintf("%d+(%d/%d)", tm.Unix(), f.Numerator, f.Denominator)
}
