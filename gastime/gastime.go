// Package gastime measures time based on the consumption of gas.
package gastime

import (
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/holiman/uint256"
)

// Time represents an instant in time, its passage measured in [gas.Gas]
// consumption. It is not thread safe nor is the zero value valid.
//
// In addition to the passage of time, it also tracks excess consumption above a
// target, as described in [ACP-194] as a "continuous" version of [ACP-176].
//
// Copying a Time, either directly or by dereferencing a pointer, will result in
// undefined behaviour. Use [Time.Clone] instead as it reestablishes internal
// invariants.
//
// [ACP-176]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/176-dynamic-evm-gas-limit-and-price-discovery-updates
// [ACP-194]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
type Time struct {
	TimeMarshaler
}

// makeTime is a constructor shared by [New] and [Time.Clone].
func makeTime(t *proxytime.Time[gas.Gas], target, excess gas.Gas) *Time {
	tm := &Time{
		TimeMarshaler: TimeMarshaler{
			Time:   t,
			target: target,
			excess: excess,
		},
	}
	tm.establishInvariants()
	return tm
}

func (tm *Time) establishInvariants() {
	tm.Time.SetRateInvariants(&tm.target, &tm.excess)
}

// New returns a new [Time], set from a Unix timestamp. The consumption of
// `2*target` units of [gas.Gas] is equivalent to a tick of 1 second.
func New(unixSeconds uint64, target, startingExcess gas.Gas) *Time {
	return makeTime(proxytime.New(unixSeconds, 2*target), target, startingExcess)
}

// Clone returns a deep copy of the time.
func (tm *Time) Clone() *Time {
	// [proxytime.Time.Clone] explicitly does NOT clone the rate invariants, so
	// we reestablish them as if we were constructing a new instance.
	return makeTime(tm.Time.Clone(), tm.target, tm.excess)
}

// Target returns the `T` parameter of ACP-176.
func (tm *Time) Target() gas.Gas {
	return tm.target
}

// Excess returns the `x` variable of ACP-176.
func (tm *Time) Excess() gas.Gas {
	return tm.excess
}

// Price returns the price of a unit of gas, i.e. the "base fee".
func (tm *Time) Price() gas.Price {
	return gas.CalculatePrice(1 /* M */, tm.excess, 87*tm.target /* K */)
}

// BaseFee is equivalent to [Time.Price], returning the result as a uint256 for
// compatibility with geth/libevm objects.
func (tm *Time) BaseFee() *uint256.Int {
	return uint256.NewInt(uint64(tm.Price()))
}

// SetTarget changes the target gas consumption per second. It is equivalent to
// [proxytime.Time.SetRate] with `2*t`, but is preferred as it avoids
// accidentally setting an odd rate.
func (tm *Time) SetTarget(t gas.Gas) {
	tm.SetRate(2 * t) // also updates target as it was passed to [proxytime.Time.SetRateInvariants]
}

// Tick is equivalent to [proxytime.Time.Tick] except that it also updates the
// gas excess.
func (tm *Time) Tick(g gas.Gas) {
	tm.Time.Tick(g)

	R, T := tm.Rate(), tm.Target()
	quo, _, _ := intmath.MulDiv(g, R-T, R) //nolint:errcheck // R-T < R so the quotient is < g
	tm.excess += quo
}

// FastForwardTo is equivalent to [proxytime.Time.FastForwardTo] except that it
// may also update the gas excess.
func (tm *Time) FastForwardTo(to uint64) {
	sec, frac := tm.Time.FastForwardTo(to)
	if sec == 0 && frac.Numerator == 0 {
		return
	}

	R, T := tm.Rate(), tm.Target()
	quo, _, _ := intmath.MulDiv(R*gas.Gas(sec)+frac.Numerator, T, R) //nolint:errcheck // T < R so the quotient is < LHS
	tm.excess = intmath.BoundedSubtract(tm.excess, quo, 0)
}
