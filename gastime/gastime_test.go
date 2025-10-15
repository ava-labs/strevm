// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"fmt"
	"math"
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/intmath"
	"github.com/ava-labs/strevm/proxytime"
)

func (tm *Time) cloneViaCanotoRoundTrip(tb testing.TB) *Time {
	tb.Helper()
	x := new(Time)
	require.NoErrorf(tb, x.UnmarshalCanoto(tm.MarshalCanoto()), "%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", tm)
	return x
}

func TestClone(t *testing.T) {
	tm := New(42, 1e6, 1e5)
	tm.Tick(1)

	if diff := cmp.Diff(tm, tm.Clone(), CmpOpt()); diff != "" {
		t.Errorf("%T.Clone() diff (-want +got):\n%s", tm, diff)
	}
	if diff := cmp.Diff(tm, tm.cloneViaCanotoRoundTrip(t), CmpOpt()); diff != "" {
		t.Errorf("%T.UnmarshalCanoto(%[1]T.MarshalCanoto()) diff (-want +got):\n%s", tm, diff)
	}
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
	tm := New(42, 1.6e6, initExcess)

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
			x, _, err := intmath.MulDiv(initExcess, wantRate, 3.2e6)
			require.NoErrorf(t, err, "intmath.MulDiv(%d, %d, %d)", initExcess, 4e6, 3.2e6)
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
	tm := New(42, rate/2, 0)

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

	tests := []struct {
		target, want gas.Gas
	}{
		{1, 87},
		{2, 174},
		{max / 87, (max / 87) * 87},
		{max/87 - 0, max - 81}, // identical to above, but explicit for clarity
		{max/87 - 1, max - 81 - 87},
		{max/87 + 1, max}, // because `max - 81 + 87` would overflow
		{max, max},
	}

	tm := New(0, 1, 0)
	for _, tt := range tests {
		require.NoErrorf(t, tm.SetTarget(tt.target), "%T.SetTarget(%v)", tm, tt.target)
		assert.Equalf(t, tt.want, tm.excessScalingFactor(), "T = %d", tt.target)
	}
}

func TestTargetClamping(t *testing.T) {
	tm := New(0, MaxTarget+1, 0)
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
