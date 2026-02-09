// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/proxytime"
)

func mustNew(tb testing.TB, at time.Time, target, startingExcess gas.Gas, gasConfig hook.GasConfig) *Time {
	tb.Helper()
	tm, err := New(at, target, startingExcess, gasConfig)
	require.NoError(tb, err, "New(%v, %d, %d, %v)", at, target, startingExcess, gasConfig)
	return tm
}

func (tm *Time) cloneViaCanotoRoundTrip(tb testing.TB) *Time {
	tb.Helper()
	x := new(Time)
	require.NoErrorf(tb, x.UnmarshalCanoto(tm.MarshalCanoto()), "%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", tm)
	return x
}

func TestClone(t *testing.T) {
	tm := mustNew(t, time.Unix(42, 0), 1e6, 1e5, hook.GasConfig{TargetToExcessScaling: 100, MinPrice: 100})

	if diff := cmp.Diff(tm, tm.Clone(), CmpOpt()); diff != "" {
		t.Errorf("%T.Clone() diff (-want +got):\n%s", tm, diff)
	}
	if diff := cmp.Diff(tm, tm.cloneViaCanotoRoundTrip(t), CmpOpt()); diff != "" {
		t.Errorf("%T.UnmarshalCanoto(%[1]T.MarshalCanoto()) diff (-want +got):\n%s", tm, diff)
	}
}

// TestUnmarshalBackwardCompatibility verifies that deserializing old data
// (without config field) applies default values for TargetToExcessScaling
// and MinPrice, ensuring backward compatibility.
func TestUnmarshalBackwardCompatibility(t *testing.T) {
	// Create a Time and serialize it
	tm := mustNew(t, time.Unix(42, 0), 1e6, 1e5, hook.DefaultGasConfig())

	// Manually create serialized data without config field by using the
	// proxytime.Time and target/excess fields only
	tmWithoutConfig := &Time{
		TimeMarshaler: TimeMarshaler{
			Time:   tm.Time,
			target: tm.target,
			excess: tm.excess,
			// config is zero-valued (no TargetToExcessScaling, no MinPrice)
		},
	}

	// Serialize the Time without config
	data := tmWithoutConfig.MarshalCanoto()

	// Deserialize - should apply defaults
	restored := new(Time)
	require.NoError(t, restored.UnmarshalCanoto(data), "UnmarshalCanoto(%v)", data)

	// Verify defaults were applied
	assert.Equal(t, gas.Gas(hook.DefaultTargetToExcessScaling), restored.config.targetToExcessScaling,
		"TargetToExcessScaling should default to %d", hook.DefaultTargetToExcessScaling)
	assert.Equal(t, hook.DefaultMinPrice, restored.config.minPrice,
		"MinPrice should default to %d", hook.DefaultMinPrice)

	// Verify the Time is functional
	assert.Equal(t, tm.target, restored.target, "target changed")
	assert.Equal(t, tm.excess, restored.excess, "excess changed")
}

// state captures parameters about a [Time] for assertion in tests. It includes
// both explicit (i.e. struct fields) and derived parameters (e.g. gas price),
// which aid testing of behaviour and invariants in a more fine-grained manner
// than direct comparison of two instances.
type state struct {
	UnixTime             uint64
	ConsumedThisSecond   proxytime.FractionalSecond[gas.Gas]
	Rate, Target, Excess gas.Gas
	Price                gas.Price
}

func (tm *Time) state() state {
	return state{
		UnixTime:           tm.Unix(),
		ConsumedThisSecond: tm.Fraction(),
		Rate:               tm.Rate(),
		Target:             tm.Target(),
		Excess:             tm.Excess(),
		Price:              tm.Price(),
	}
}

func (tm *Time) requireState(tb testing.TB, desc string, want state, opts ...cmp.Option) {
	tb.Helper()
	if diff := cmp.Diff(want, tm.state(), opts...); diff != "" {
		tb.Fatalf("%s (-want +got):\n%s", desc, diff)
	}
}

func TestNew(t *testing.T) {
	frac := func(num, den gas.Gas) (f proxytime.FractionalSecond[gas.Gas]) {
		f.Numerator = num
		f.Denominator = den
		return
	}

	ignore := cmpopts.IgnoreFields(state{}, "Rate", "Price")

	tests := []struct {
		name           string
		unix, nanos    int64
		target, excess gas.Gas
		want           state
	}{
		{
			name:   "rate at nanosecond resolution",
			unix:   42,
			nanos:  123_456,
			target: 1e9 / TargetToRate,
			want: state{
				UnixTime:           42,
				ConsumedThisSecond: frac(123_456, 1e9),
				Target:             1e9 / TargetToRate,
			},
		},
		{
			name:   "scaling in constructor not applied to starting excess",
			unix:   100,
			nanos:  TargetToRate,
			target: 50 / TargetToRate,
			excess: 987_654,
			want: state{
				UnixTime:           100,
				ConsumedThisSecond: frac(1, 50),
				Target:             50 / TargetToRate,
				Excess:             987_654,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := time.Unix(tt.unix, tt.nanos)
			got := mustNew(t, tm, tt.target, tt.excess, hook.DefaultGasConfig())
			got.requireState(t, fmt.Sprintf("New(%v, %d, %d)", tm, tt.target, tt.excess), tt.want, ignore)
		})
	}
}

func (tm *Time) mustSetRate(tb testing.TB, rate gas.Gas) {
	tb.Helper()
	require.NoErrorf(tb, tm.SetRate(rate), "%T.%T.SetRate(%d)", tm, TimeMarshaler{}, rate)
}

func (tm *Time) mustSetTarget(tb testing.TB, target gas.Gas) {
	tb.Helper()
	require.NoError(tb, tm.SetTarget(target), "%T.SetTarget(%d)", tm, target)
}

func TestScaling(t *testing.T) {
	const initExcess = gas.Gas(1_234_567_890)
	tm := mustNew(t, time.Unix(42, 0), 1.6e6, initExcess, hook.DefaultGasConfig())

	// The initial price isn't important in this test; what we care about is
	// that it's invariant under scaling of the target etc.
	initPrice := tm.Price()
	if initPrice == 1 {
		t.Fatalf("Bad test setup: increase initial excess to achieve %T > 1", initPrice)
	}

	ignore := cmpopts.IgnoreFields(state{}, "UnixTime", "ConsumedThisSecond")

	tm.requireState(t, "initial", state{
		Rate:   3.2e6,
		Target: 1.6e6,
		Excess: initExcess,
		Price:  initPrice,
	}, ignore)

	tm.mustSetTarget(t, 3.2e6)
	tm.requireState(t, "after SetTarget()", state{
		Rate:   6.4e6,
		Target: 3.2e6,
		Excess: 2 * initExcess,
		Price:  initPrice, // unchanged
	}, ignore)

	// SetRate is identical to setting via the target, as long as the rate is
	// even. Although the documentation states that SetTarget is preferred, we
	// still need to test SetRate.
	const (
		wantTargetViaRate = 2e6
		wantRate          = wantTargetViaRate * TargetToRate
	)
	want := state{
		Rate:   wantRate,
		Target: wantTargetViaRate,
		Excess: (func() gas.Gas {
			// Scale the _initial_ excess relative to the new and _initial_
			// rates, not the most recent rate before scaling.
			x, _, err := intmath.MulDivCeil(initExcess, wantRate, 3.2e6)
			require.NoErrorf(t, err, "intmath.MulDivCeil(%d, %d, %d)", initExcess, 4e6, 3.2e6)
			return x
		})(),
		Price: initPrice, // unchanged
	}
	for roundingError := range gas.Gas(TargetToRate) {
		r := wantRate + roundingError
		tm.mustSetRate(t, r)
		tm.requireState(t, fmt.Sprintf("after SetRate(%d)", r), want, ignore)
	}

	testPostClone := func(t *testing.T, cloned *Time) {
		t.Helper()
		want := want
		cloned.requireState(t, "unchanged immediately after clone", want, ignore)

		cloned.mustSetRate(t, cloned.Rate()*2)
		tm.requireState(t, "original Time unchanged by setting clone's rate", want, ignore)

		want.Rate *= 2
		want.Target *= 2
		want.Excess *= 2
		cloned.requireState(t, "scaling after clone and then SetRate()", want, ignore)
	}

	t.Run("clone", func(t *testing.T) {
		testPostClone(t, tm.Clone())
	})

	t.Run("canoto_roundtrip", func(t *testing.T) {
		testPostClone(t, tm.cloneViaCanotoRoundTrip(t))
	})
}

func TestExcess(t *testing.T) {
	const rate = gas.Gas(3.2e6)
	tm := mustNew(t, time.Unix(42, 0), rate/2, 0, hook.DefaultGasConfig())

	frac := func(num gas.Gas) (f proxytime.FractionalSecond[gas.Gas]) {
		f.Numerator = num
		f.Denominator = rate
		return f
	}

	ignore := cmpopts.IgnoreFields(state{}, "Rate", "Target", "Price")

	tm.requireState(t, "initial", state{
		UnixTime:           42,
		ConsumedThisSecond: frac(0),
		Excess:             0,
	}, ignore)

	// NOTE: when R = 2T, excess increases or decreases by half the passage of
	// time, depending on whether time was Tick()ed or FastForward()ed,
	// respectively.

	steps := []struct {
		desc string
		// Only one of fast-forwarding or ticking per step.
		ffToBefore     uint64
		ffToBeforeFrac gas.Gas
		tickBefore     gas.Gas
		want           state
	}{
		{
			desc:       "initial tick 1/2s",
			tickBefore: rate / 2,
			want: state{
				UnixTime:           42,
				ConsumedThisSecond: frac(rate / 2),
				Excess:             (rate / 2) / 2,
			},
		},
		{
			desc:       "total tick 3/4s",
			tickBefore: rate / 4,
			want: state{
				UnixTime:           42,
				ConsumedThisSecond: frac(3 * rate / 4),
				Excess:             3 * rate / 8,
			},
		},
		{
			desc:       "total tick 5/4s",
			tickBefore: rate / 2,
			want: state{
				UnixTime:           43,
				ConsumedThisSecond: frac(rate / 4),
				Excess:             5 * rate / 8,
			},
		},
		{
			desc:       "total tick 11.25s",
			tickBefore: 10 * rate,
			want: state{
				UnixTime:           53,
				ConsumedThisSecond: frac(rate / 4),
				Excess:             45 * rate / 8, // (11*4 + 1) quarters of ticking, halved
			},
		},
		{
			desc:       "no op fast forward",
			ffToBefore: 53,
			want: state{ // unchanged
				UnixTime:           53,
				ConsumedThisSecond: frac(rate / 4),
				Excess:             45 * rate / 8,
			},
		},
		{
			desc:       "fast forward 11.25s to 13s",
			ffToBefore: 55,
			want: state{
				UnixTime:           55,
				ConsumedThisSecond: frac(0),
				Excess:             45*rate/8 - 7*rate/8,
			},
		},
		{
			desc:           "fast forward 0.5s to 13.5s",
			ffToBefore:     55,
			ffToBeforeFrac: rate / 2,
			want: state{
				UnixTime:           55,
				ConsumedThisSecond: frac(rate / 2),
				Excess:             45*rate/8 - 7*rate/8 - (rate/2)/2,
			},
		},
		{
			desc:           "fast forward 0.75s to 14.25s",
			ffToBefore:     56,
			ffToBeforeFrac: rate / 4,
			want: state{
				UnixTime:           56,
				ConsumedThisSecond: frac(rate / 4),
				Excess:             45*rate/8 - 7*rate/8 - (rate/2)/2 - 3*(rate/4)/2,
			},
		},
		{
			desc:       "fast forward causes overflow when seconds multiplied by R",
			ffToBefore: math.MaxUint64,
			want: state{
				UnixTime:           math.MaxUint64,
				ConsumedThisSecond: frac(0),
				Excess:             0,
			},
		},
	}

	for _, s := range steps {
		ffSec, ffFrac := s.ffToBefore, s.ffToBeforeFrac
		tick := s.tickBefore

		switch ff := (ffSec > 0 || ffFrac > 0); {
		case ff && tick > 0:
			t.Fatalf("Bad test setup (%q) only FastForward() or Tick() before", s.desc)
		case ff:
			tm.FastForwardTo(ffSec, ffFrac)
		case tick > 0:
			tm.Tick(tick)
		}
		tm.requireState(t, s.desc, s.want, ignore)
	}
}

func TestExcessScalingFactor(t *testing.T) {
	const max = math.MaxUint64

	defaultTests := []struct {
		target, want gas.Gas
	}{
		// Default scaling (87)
		{1, 87},
		{2, 174},
		{max / 87, (max / 87) * 87},
		{max/87 - 0, max - 81}, // identical to above, but explicit for clarity
		{max/87 - 1, max - 81 - 87},
		{max/87 + 1, max}, // because `max - 81 + 87` would overflow
		{max, max},        // target clamped to MaxTarget, still overflows
	}
	t.Run("default", func(t *testing.T) {
		tm := mustNew(t, time.Unix(0, 0), 1, 0, hook.DefaultGasConfig())
		for _, tt := range defaultTests {
			require.NoErrorf(t, tm.SetTarget(tt.target), "%T.SetTarget(%v)", tm, tt.target)
			assert.Equalf(t, tt.want, tm.excessScalingFactor(), "T=%d", tt.target)
		}
	})

	customTests := []struct {
		scaling, target, want gas.Gas
	}{
		// Scaling = 1 (minimum meaningful value)
		{1, 1, 1},
		{1, 100, 100},
		{1, MaxTarget, MaxTarget}, // target clamped, scaling=1 so result = MaxTarget

		// Scaling = 100
		{100, 1, 100},
		{100, 10, 1000},
		{100, max / 100, (max / 100) * 100},
		{100, max/100 + 1, max}, // overflow

		// Scaling = 50 (lower than default, price more sensitive)
		{50, 1, 50},
		{50, 1_000_000, 50_000_000},
		{50, max / 50, (max / 50) * 50},
		{50, max/50 + 1, max},

		// Large scaling values
		{1000, 1, 1000},
		{1000, max / 1000, (max / 1000) * 1000},
		{1000, max/1000 + 1, max},

		// Edge case: scaling = max
		{max, 1, max},
		{max, 2, max}, // would overflow
	}

	t.Run("custom scaling", func(t *testing.T) {
		for _, tt := range customTests {
			tm := mustNew(t, time.Unix(0, 0), tt.target, 0, hook.GasConfig{TargetToExcessScaling: tt.scaling, MinPrice: hook.DefaultMinPrice})
			require.NoErrorf(t, tm.SetTarget(tt.target), "%T.SetTarget(%v)", tm, tt.target)
			assert.Equalf(t, tt.want, tm.excessScalingFactor(), "scaling=%d, T=%d", tt.scaling, tt.target)
		}
	})
}

func TestMinPrice(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		tests := []struct {
			target, excess gas.Gas
			want           gas.Price
		}{
			// Zero excess returns DefaultMinPrice
			{1_000_000, 0, 1},
			{10_000_000, 0, 1},
			// With excess, price > minPrice but still uses default minPrice as base
			{1_000_000, 1_000_000, 1},   // small excess, rounds to 1
			{1_000_000, 100_000_000, 3}, // excess/K ≈ 1.15, e^1.15 ≈ 3.16
		}
		for _, tt := range tests {
			tm := mustNew(t, time.Unix(0, 0), tt.target, tt.excess, hook.DefaultGasConfig())
			assert.Equalf(t, tt.want, tm.Price(), "target=%d, excess=%d", tt.target, tt.excess)
		}
	})

	t.Run("custom min price", func(t *testing.T) {
		tests := []struct {
			minPrice       gas.Price
			target, excess gas.Gas
			want           gas.Price
		}{
			// Zero excess returns exactly minPrice
			{1, 1_000_000, 0, 1},
			{10, 1_000_000, 0, 10},
			{100, 1_000_000, 0, 100},
			{1000, 1_000_000, 0, 1000},

			// With excess, price is minPrice * e^(excess/K)
			{10, 1_000_000, 87_000_000, 27},   // excess/K = 1, e^1 ≈ 2.718, 10*2.718 ≈ 27
			{100, 1_000_000, 87_000_000, 271}, // 100 * e^1 ≈ 271
		}
		for _, tt := range tests {
			tm := mustNew(t, time.Unix(0, 0), tt.target, tt.excess, hook.GasConfig{TargetToExcessScaling: hook.DefaultTargetToExcessScaling, MinPrice: tt.minPrice})
			assert.Equalf(t, tt.want, tm.Price(), "minPrice=%d, target=%d, excess=%d", tt.minPrice, tt.target, tt.excess)
		}
	})

	t.Run("min price updated via SetOpts", func(t *testing.T) {
		tm := mustNew(t, time.Unix(0, 0), 1_000_000, 0, hook.DefaultGasConfig())
		assert.Equal(t, hook.DefaultMinPrice, tm.Price(), "initial price with default minPrice")

		cfg := hook.DefaultGasConfig()
		cfg.MinPrice = 100
		require.NoError(t, tm.SetConfig(cfg))
		assert.Equal(t, gas.Price(100), tm.Price(), "price after SetOpts(WithMinPrice(100))")

		cfg.MinPrice = 1000
		require.NoError(t, tm.SetConfig(cfg))
		assert.Equal(t, gas.Price(1000), tm.Price(), "price after SetOpts(WithMinPrice(1000))")

		// Verify SetConfig rejects zero
		cfg.MinPrice = 0
		err := tm.SetConfig(cfg)
		require.Error(t, err, "SetOpts should reject zero min price")
		assert.Equal(t, gas.Price(1000), tm.Price(), "price should be unchanged after rejected update")
	})
}

func TestTargetClamping(t *testing.T) {
	tm := mustNew(t, time.Unix(0, 0), MaxTarget+1, 0, hook.DefaultGasConfig())
	require.Equal(t, MaxTarget, tm.Target(), "tm.Target() clamped by constructor")

	tests := []struct {
		setTo, want gas.Gas
	}{
		{setTo: 10, want: 10},
		{setTo: MaxTarget + 1, want: MaxTarget},
		{setTo: 20, want: 20},
		{setTo: math.MaxUint64, want: MaxTarget},
	}

	for _, tt := range tests {
		require.NoErrorf(t, tm.SetTarget(tt.setTo), "%T.SetTarget(%d)", tm, tt.setTo)
		assert.Equalf(t, tt.want, tm.Target(), "%T.Target() after setting to %#x", tm, tt.setTo)
		assert.Equalf(t, tm.Target()*TargetToRate, tm.Rate(), "%T.Rate() == %d * %[1]T.Target()", tm, TargetToRate)
	}
}
