// Package proxytime measures time based on a proxy unit and associated unit
// rate.
package proxytime

import (
	"time"

	"github.com/ava-labs/strevm/intmath"
)

// A Duration is a type parameter for use as the unit of passage of [Time].
type Duration interface {
	~uint64
}

// Time represents an instant in time, its passage measured by an arbitrary unit
// of duration. It is not thread safe nor is the zero value valid.
type Time[D Duration] struct {
	seconds         uint64
	fraction, hertz D // invariant: fraction < hertz
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

// New returns a new [Time], set from a Unix timestamp. The passage of `hertz`
// units is equivalent to a tick of 1 second.
func New[D Duration](unixSeconds uint64, hertz D) *Time[D] {
	return &Time[D]{
		seconds: unixSeconds,
		hertz:   hertz,
	}
}

// Copy returns a copy of the time.
func (tm *Time[D]) Copy() *Time[D] {
	t := *tm
	return &t
}

// Tick advances the time by `d`.
func (tm *Time[D]) Tick(d D) {
	tm.fraction += d
	tm.seconds += uint64(tm.fraction / tm.hertz)
	tm.fraction %= tm.hertz
}

// SetRate changes the unit rate at which time passes. The requisite integer
// division may result in rounding down of the fractional-second component of
// time, the amount of which is returned.
func (tm *Time[D]) SetRate(hertz D) (truncated FractionalSecond[D]) {
	truncated.Denominator = tm.hertz
	tm.fraction, truncated.Numerator = intmath.MulDiv(tm.fraction, hertz, tm.hertz)
	tm.hertz = hertz
	return truncated
}

// Rate returns the proxy duration required for the passage of one second.
func (tm *Time[D]) Rate() D {
	return tm.hertz
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
