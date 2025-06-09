// Package proxytime measures time based on a proxy unit and associated unit
// rate.
package proxytime

import (
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
func (tm *Time[D]) Unix() uint64 { return tm.seconds }

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
	tm.fraction += d
	tm.seconds += uint64(tm.fraction / tm.hertz)
	tm.fraction %= tm.hertz
}

// FastForwardTo sets the time to the specified Unix timestamp if it is in the
// future, returning the integer and fraction number of seconds by which the
// time was advanced.
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
func (tm *Time[D]) SetRate(hertz D) (truncated FractionalSecond[D]) {
	tm.fraction, truncated = tm.scale(tm.fraction, hertz)

	for _, v := range tm.rateInvariants {
		*v, _ = tm.scale(*v, hertz)
	}

	tm.hertz = hertz
	return truncated
}

// SetRateInvariants sets units that, whenever [Time.SetRate] is called, will be
// scaled relative to the change in rate. Scaling may be affected by the same
// truncation described for [Time.SetRate]. Truncation aside, the rational
// numbers formed by the invariants divided by the rate will each remain equal
// despite their change in denominator.
func (tm *Time[D]) SetRateInvariants(inv ...*D) {
	tm.rateInvariants = inv
}

func (tm *Time[D]) scale(val, newRate D) (scaled D, truncated FractionalSecond[D]) {
	truncated.Denominator = tm.hertz
	scaled, truncated.Numerator = intmath.MulDiv(val, newRate, tm.hertz)
	return scaled, truncated
}

// Cmp returns
//
//	-1 if tm is before u
//	 0 if tm and u represent the same instant
//	+1 if tm is after u.
//
// Results are undefined if [Time.Rate] is different for the two instants.
func (tm *Time[D]) Cmp(u *Time[D]) int {
	if ts, us := tm.seconds, u.seconds; ts < us {
		return -1
	} else if ts > us {
		return 1
	}

	if tf, uf := tm.fraction, u.fraction; tf < uf {
		return -1
	} else if tf > uf {
		return 1
	}
	return 0
}

// AsTime converts the proxy time to a standard [time.Time] in UTC. AsTime is
// analogous to setting a rate of 1e9 (nanosecond), which might result in
// truncation.
func (tm *Time[D]) AsTime() time.Time {
	nsec, _ /*remainder*/ := intmath.MulDiv(tm.fraction, 1e9, tm.hertz)
	return time.Unix(int64(tm.seconds), int64(nsec)).In(time.UTC)
}
