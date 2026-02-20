// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gastime measures time based on the consumption of gas.
package gastime

import (
	"math"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/hook"
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
func makeTime(t *proxytime.Time[gas.Gas], target, excess gas.Gas, c config) *Time {
	tm := &Time{
		TimeMarshaler: TimeMarshaler{
			Time:   t,
			target: target,
			excess: excess,
			config: c,
		},
	}
	tm.establishInvariants()
	return tm
}

func (tm *Time) establishInvariants() {
	tm.Time.SetRateInvariants(&tm.target, &tm.excess)
}

// New returns a new [Time], derived from a [time.Time]. The consumption of
// `target` * [TargetToRate] units of [gas.Gas] is equivalent to a tick of 1
// second. Targets are clamped to [MaxTarget]. The gasPriceConfig parameter
// specifies the gas pricing configurations.
func New(at time.Time, target, startingExcess gas.Gas, gasPriceConfig hook.GasPriceConfig) (*Time, error) {
	cfg, err := newConfig(gasPriceConfig)
	if err != nil {
		return nil, err
	}
	target = clampTarget(target)
	tm := proxytime.Of[gas.Gas](at)
	// [proxytime.Time.SetRate] is documented as never returning an error when
	// no invariants have been registered.
	_ = tm.SetRate(rateOf(target))
	return makeTime(tm, target, startingExcess, cfg), nil
}

// SubSecond scales the value returned by [hook.Points.SubSecondBlockTime] to
// reflect the given gas rate.
func SubSecond(hooks hook.Points, hdr *types.Header, rate gas.Gas) gas.Gas {
	// [hook.Points.SubSecondBlockTime] is required to return values in
	// [0,second). The lower bound guarantees that the conversion to unsigned
	// [gas.Gas] is safe while the upper bound guarantees that the mul-div
	// result can't overflow so we don't have to check the error.
	g, _, _ := intmath.MulDivCeil(
		gas.Gas(hooks.SubSecondBlockTime(hdr)), //nolint:gosec // See above
		rate,
		gas.Gas(time.Second),
	)
	return g
}

// TargetToRate is the ratio between [Time.Target] and [proxytime.Time.Rate].
const TargetToRate = 2

// DefaultTargetToExcessScaling is the default ratio between gas target and the
// reciprocal of the excess coefficient used in price calculation (K variable in ACP-176).
const DefaultTargetToExcessScaling = 87

// DefaultMinPrice is the default minimum gas price (base fee), i.e. the M
// parameter in ACP-176's price calculation.
const DefaultMinPrice gas.Price = 1

// DefaultGasPriceConfig returns the default [hook.GasPriceConfig] values.
func DefaultGasPriceConfig() hook.GasPriceConfig {
	return hook.GasPriceConfig{
		TargetToExcessScaling: DefaultTargetToExcessScaling,
		MinPrice:              DefaultMinPrice,
		StaticPricing:         false,
	}
}

// MaxTarget is the maximum allowable [Time.Target] to avoid overflows of the
// associated [proxytime.Time.Rate]. Values above this are silently clamped.
const MaxTarget = gas.Gas(math.MaxUint64 / TargetToRate)

func rateOf(target gas.Gas) gas.Gas { return target * TargetToRate }
func clampTarget(t gas.Gas) gas.Gas { return min(t, MaxTarget) }
func roundRate(r gas.Gas) gas.Gas   { return (r / TargetToRate) * TargetToRate }

// SafeRateOfTarget returns the corresponding rate for the given gas target,
// protecting against overflow. It is equivalent to the product of
// [TargetToRate] and the minimum of [MaxTarget] and the argument.
func SafeRateOfTarget(target gas.Gas) gas.Gas {
	return rateOf(clampTarget(target))
}

// Clone returns a deep copy of the time.
func (tm *Time) Clone() *Time {
	// [proxytime.Time.Clone] explicitly does NOT clone the rate invariants, so
	// we reestablish them as if we were constructing a new instance.
	return makeTime(tm.Time.Clone(), tm.target, tm.excess, tm.config)
}

// Target returns the `T` parameter of ACP-176.
func (tm *Time) Target() gas.Gas {
	return tm.target
}

// Excess returns the `x` variable of ACP-176.
func (tm *Time) Excess() gas.Gas {
	return tm.excess
}

// Price returns the price of a unit of gas, i.e. the "base fee", determined by
// [gas.CalculatePrice]. However, when [hook.GasPriceConfig.StaticPricing] is
// true, Price always returns [hook.GasPriceConfig.MinPrice].
func (tm *Time) Price() gas.Price {
	if tm.config.staticPricing {
		return tm.config.minPrice
	}
	return gas.CalculatePrice(tm.config.minPrice, tm.excess, tm.excessScalingFactor())
}

// excessScalingFactor returns the K variable of ACP-103/176, i.e.
// [config.targetToExcessScaling] * T, capped at [math.MaxUint64].
func (tm *Time) excessScalingFactor() gas.Gas {
	return intmath.BoundedMultiply(tm.config.targetToExcessScaling, tm.target, math.MaxUint64)
}

// BaseFee is equivalent to [Time.Price], returning the result as a uint256 for
// compatibility with geth/libevm objects.
func (tm *Time) BaseFee() *uint256.Int {
	return uint256.NewInt(uint64(tm.Price()))
}

// SetRate changes the gas rate per second, rounding down the argument if it is
// not a multiple of [TargetToRate]. See [Time.SetTarget] re potential error(s).
func (tm *Time) SetRate(r gas.Gas) error {
	return tm.TimeMarshaler.SetRate(roundRate(r))
}

// SetTarget changes the target gas consumption per second, clamping the
// argument to [MaxTarget]. It returns an error if the scaled [Time.Excess]
// overflows as a result of the scaling.
func (tm *Time) SetTarget(t gas.Gas) error {
	return tm.TimeMarshaler.SetRate(rateOf(clampTarget(t))) // also updates [Time.Target] as it was passed to [proxytime.Time.SetRateInvariants]
}

// setConfig sets the full config with excess scaling to maintain price continuity.
//
// When config changes, excess is scaled to maintain price continuity:
//   - K changes (via TargetToExcessScaling): Scale excess to maintain current price
//   - StaticPricing is true: Set excess to 0, enable fixed price mode and update config.
//   - M decreases: Scale excess to maintain current price
//   - M increases AND current price >= new M: Scale excess to maintain current price
//   - M increases AND current price < new M: Price bumps to new M (excess becomes 0)
func (tm *Time) setConfig(cfg hook.GasPriceConfig, reinstateIfConfigChanges gas.Price) error {
	newCfg, err := newConfig(cfg)
	if err != nil {
		return err
	}
	if newCfg.equals(tm.config) {
		return nil
	}
	tm.config = newCfg
	tm.excess = tm.findExcessForPrice(reinstateIfConfigChanges)
	return nil
}

// findExcessForPrice uses binary search over uint64 to find the smallest excess
// value that produces targetPrice with the current [config].
func (tm *Time) findExcessForPrice(targetPrice gas.Price) gas.Gas {
	// We return 0 in case targetPrice < minPrice because we should at least maintain the minimum price
	// by setting the excess to 0. ( P = M * e^(0 / K) = M )
	// Note: Even though we return 0 for excess it won't avoid accumulating excess in the long run.
	if targetPrice <= tm.config.minPrice || tm.config.staticPricing {
		return 0
	}

	k := tm.excessScalingFactor()

	// The price function is monotonic non-decreasing so binary search is appropriate.
	lo, hi := gas.Gas(0), gas.Gas(math.MaxUint64)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if gas.CalculatePrice(tm.config.minPrice, mid, k) >= targetPrice {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
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
