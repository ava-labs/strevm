// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gastime measures time based on the consumption of gas.
package gastime

import (
	"math"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/proxytime"
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
func makeTime(t *proxytime.Time[gas.Gas], target, excess, targetToExcessScaling gas.Gas, minPrice gas.Price) *Time {
	tm := &Time{
		TimeMarshaler: TimeMarshaler{
			Time:                  t,
			target:                target,
			excess:                excess,
			targetToExcessScaling: targetToExcessScaling,
			minPrice:              minPrice,
		},
	}
	tm.establishInvariants()
	return tm
}

func (tm *Time) establishInvariants() {
	tm.Time.SetRateInvariants(&tm.target, &tm.excess)
}

// An Option configures the [Time] created by [New].
type Option = options.Option[config]

type config struct {
	targetToExcessScaling gas.Gas
	minPrice              gas.Price
}

// WithTargetToExcessScaling overrides the default target to excess scaling ratio.
func WithTargetToExcessScaling(s gas.Gas) Option {
	return options.Func[config](func(c *config) {
		c.targetToExcessScaling = s
	})
}

// WithMinPrice overrides the default minimum gas price.
func WithMinPrice(p gas.Price) Option {
	return options.Func[config](func(c *config) {
		c.minPrice = p
	})
}

// New returns a new [Time], set from a Unix timestamp. The consumption of
// `target` * [TargetToRate] units of [gas.Gas] is equivalent to a tick of 1
// second. Targets are clamped to [MaxTarget]. The minPrice and
// targetToExcessScaling parameters default to [DefaultMinPrice] and
// [DefaultTargetToExcessScaling] respectively, but can be overridden with
// [WithMinPrice] and [WithTargetToExcessScaling].
func New(unixSeconds uint64, target, startingExcess gas.Gas, opts ...Option) *Time {
	cfg := &config{
		targetToExcessScaling: DefaultTargetToExcessScaling,
		minPrice:              DefaultMinPrice,
	}
	options.ApplyTo(cfg, opts...)
	target = clampTarget(target)
	return makeTime(proxytime.New(unixSeconds, rateOf(target)), target, startingExcess, cfg.targetToExcessScaling, cfg.minPrice)
}

// TargetToRate is the ratio between [Time.Target] and [proxytime.Time.Rate].
const TargetToRate = 2

// DefaultTargetToExcessScaling is the default ratio between [Time.Target] and
// the reciprocal of the [Time.Excess] coefficient used in calculating
// [Time.Price]. In [ACP-176] this is the K variable.
//
// [ACP-176]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/176-dynamic-evm-gas-limit-and-price-discovery-updates
const DefaultTargetToExcessScaling = 87

// DefaultMinPrice is the default minimum gas price (base fee). This is the M
// parameter in ACP-176's price calculation.
const DefaultMinPrice gas.Price = 1

// MaxTarget is the maximum allowable [Time.Target] to avoid overflows of the
// associated [proxytime.Time.Rate]. Values above this are silently clamped.
const MaxTarget = gas.Gas(math.MaxUint64 / TargetToRate)

func rateOf(target gas.Gas) gas.Gas { return target * TargetToRate }
func clampTarget(t gas.Gas) gas.Gas { return min(t, MaxTarget) }
func roundRate(r gas.Gas) gas.Gas   { return (r / TargetToRate) * TargetToRate }

// Clone returns a deep copy of the time.
func (tm *Time) Clone() *Time {
	// [proxytime.Time.Clone] explicitly does NOT clone the rate invariants, so
	// we reestablish them as if we were constructing a new instance.
	return makeTime(tm.Time.Clone(), tm.target, tm.excess, tm.targetToExcessScaling, tm.minPrice)
}

// Target returns the `T` parameter of ACP-176.
func (tm *Time) Target() gas.Gas {
	return tm.target
}

// Excess returns the `x` variable of ACP-176.
func (tm *Time) Excess() gas.Gas {
	return tm.excess
}

// TargetToExcessScaling returns the ratio between [Time.Target] and the
// reciprocal of the [Time.Excess] coefficient used in calculating [Time.Price].
// In [ACP-176] this is the K variable.
func (tm *Time) TargetToExcessScaling() gas.Gas {
	return tm.targetToExcessScaling
}

// MinPrice returns the minimum gas price (base fee), i.e. the M parameter in
// ACP-176's price calculation.
func (tm *Time) MinPrice() gas.Price {
	return tm.minPrice
}

// SetTargetToExcessScaling updates the target to excess scaling ratio.
func (tm *Time) SetTargetToExcessScaling(s gas.Gas) {
	tm.targetToExcessScaling = s
}

// SetMinPrice updates the minimum gas price.
func (tm *Time) SetMinPrice(p gas.Price) {
	tm.minPrice = p
}

// Price returns the price of a unit of gas, i.e. the "base fee".
func (tm *Time) Price() gas.Price {
	// TODO (cey): Should we verify this is non-zero?
	return gas.CalculatePrice(tm.minPrice, tm.excess, tm.excessScalingFactor())
}

// excessScalingFactor returns the K variable of ACP-103/176, i.e.
// [config.targetToExcessScaling] * T, capped at [math.MaxUint64].
func (tm *Time) excessScalingFactor() gas.Gas {
	// TODO (cey): Should we verify this is non-zero instead?
	if tm.targetToExcessScaling == 0 {
		return math.MaxUint64
	}
	overflowThreshold := math.MaxUint64 / tm.targetToExcessScaling
	if tm.target > overflowThreshold {
		return math.MaxUint64
	}
	return tm.targetToExcessScaling * tm.target
}

// BaseFee is equivalent to [Time.Price], returning the result as a uint256 for
// compatibility with geth/libevm objects.
func (tm *Time) BaseFee() *uint256.Int {
	return uint256.NewInt(uint64(tm.Price()))
}

// SetRate changes the gas rate per second, rounding down the argument if it is
// not a multiple of [TargetToRate]. See [Time.SetTarget] re potential error(s).
func (tm *Time) SetRate(r gas.Gas) error {
	_, err := tm.TimeMarshaler.SetRate(roundRate(r))
	return err
}

// SetTarget changes the target gas consumption per second, clamping the
// argument to [MaxTarget]. It returns an error if the scaled [Time.Excess]
// overflows as a result of the scaling.
func (tm *Time) SetTarget(t gas.Gas) error {
	_, err := tm.TimeMarshaler.SetRate(rateOf(clampTarget(t))) // also updates [Time.Target] as it was passed to [proxytime.Time.SetRateInvariants]
	return err
}

// Tick is equivalent to [proxytime.Time.Tick] except that it also updates the
// gas excess.
func (tm *Time) Tick(g gas.Gas) {
	tm.Time.Tick(g)

	R, T := tm.Rate(), tm.Target()
	quo, _, _ := intmath.MulDiv(g, R-T, R) // overflow is impossible as (R-T)/R < 1
	tm.excess += quo
}

// FastForwardTo is equivalent to [proxytime.Time.FastForwardTo] except that it
// may also update the gas excess.
func (tm *Time) FastForwardTo(to uint64, toFrac gas.Gas) {
	sec, frac := tm.Time.FastForwardTo(to, toFrac)
	if sec == 0 && frac.Numerator == 0 {
		return
	}

	R, T := tm.Rate(), tm.Target()

	// Excess is reduced by the amount of gas skipped (g), multiplied by T/R.
	// However, to avoid overflow, the implementation needs to be a bit more
	// complicated. The reduction in excess can be calculated as follows (math
	// notation, not code, and ignoring the bounding at zero):
	//
	// s := seconds fast-forwarded (`sec`)
	// f := `frac.Numerator`
	// x := excess
	//
	// dx = -g·T/R
	// = -(sR + f)·T/R
	// = -sR·T/R - fT/R
	// = -sT - fT/R
	//
	// Note that this is equivalent to the ACP reduction of T·dt because dt is
	// equal to s + f/R since `frac.Denominator == R` is a documented invariant.
	// Therefore dx = -(s + f/R)·T, but we separate the terms differently for
	// our implementation.

	// -sT
	if s := gas.Gas(sec); tm.excess/T >= s { // sT <= x; division is safe because T > 0
		tm.excess -= s * T
	} else { // sT > x
		tm.excess = 0
	}

	// -fT/R
	quo, _, _ := intmath.MulDiv(frac.Numerator, T, R) // overflow is impossible as T/R < 1
	tm.excess = intmath.BoundedSubtract(tm.excess, quo, 0)
}
