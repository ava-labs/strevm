// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proxytime

import (
	"cmp"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func frac(num, den uint64) FractionalSecond[uint64] {
	return FractionalSecond[uint64]{Numerator: num, Denominator: den}
}

func (tm *Time[D]) assertEq(tb testing.TB, desc string, seconds int64, fraction FractionalSecond[D]) {
	tb.Helper()
	if tm.Unix() != seconds || tm.Fraction() != fraction {
		tb.Errorf("%s got (seconds, fraction) = (%d, %v); want (%d, %v)", desc, tm.Unix(), tm.Fraction(), seconds, fraction)
	}
}

func (tm *Time[D]) requireEq(tb testing.TB, desc string, seconds int64, fraction FractionalSecond[D]) {
	tb.Helper()
	before := tb.Failed()
	tm.assertEq(tb, desc, seconds, fraction)
	if !before && tb.Failed() {
		tb.FailNow()
	}
}

func TestTickAndCmp(t *testing.T) {
	const rate = 500
	tm := New(0, uint64(500))
	tm.assertEq(t, "New(0, ...)", 0, frac(0, rate))

	steps := []struct {
		tick         uint64
		wantSeconds  int64
		wantFraction uint64
	}{
		{100, 0, 100},
		{0, 0, 100},
		{399, 0, 499},
		{1, 1, 0},
		{500, 2, 0},
		{400, 2, 400},
		{200, 3, 100},
		{1600, 6, 200},
		{299, 6, 499},
		{2, 7, 1},
		{499, 8, 0},
	}

	var ticked uint64
	for _, s := range steps {
		old := tm.Clone()

		tm.Tick(s.tick)
		ticked += s.tick
		tm.requireEq(t, fmt.Sprintf("%+d", ticked), s.wantSeconds, frac(s.wantFraction, rate))

		if got, want := tm.Cmp(old), cmp.Compare(s.tick, 0); got != want {
			t.Errorf("After %T.Tick(%d); ticked.Cmp(original) got %d; want %d", tm, s.tick, got, want)
		}
		if got, want := old.Cmp(tm), cmp.Compare(0, s.tick); got != want {
			t.Errorf("After %T.Tick(%d); original.Cmp(ticked) got %d; want %d", tm, s.tick, got, want)
		}
	}
}

func TestSetRate(t *testing.T) {
	const (
		initSeconds = 42
		divisor     = 3
		initRate    = uint64(1000 * divisor)
	)
	tm := New(initSeconds, initRate)

	const tick = uint64(100 * divisor)
	tm.Tick(tick)
	tm.requireEq(t, "baseline", initSeconds, frac(tick, initRate))

	const initInvariant = 200 * divisor
	invariant := uint64(initInvariant)
	tm.SetRateInvariants(&invariant)

	steps := []struct {
		newRate, wantFraction uint64
		wantTruncated         FractionalSecond[uint64]
		wantInvariant         uint64
	}{
		{initRate / divisor, tick / divisor, frac(0, 1), invariant / divisor}, // no rounding
		{initRate * 5, tick * 5, frac(0, 1), invariant * 5},                   // multiplication never has rounding
		{15_000, 1_500, frac(0, 1), 3_000},                                    // same as above, but shows the numbers
		{75, 7 /*7.5*/, frac(7_500, 15_000), 15},                              // rounded down by 0.5, denominated in the old rate
	}

	for _, s := range steps {
		old := tm.Rate()
		gotTruncated, err := tm.SetRate(s.newRate)
		require.NoErrorf(t, err, "%T.SetRate(%d)", tm, s.newRate)
		tm.requireEq(t, fmt.Sprintf("rate changed from %d to %d", old, s.newRate), initSeconds, frac(s.wantFraction, s.newRate))

		if gotTruncated.Numerator == 0 && s.wantTruncated.Numerator == 0 {
			assert.NotZerof(t, gotTruncated.Denominator, "truncation %T.Denominator with 0 numerator", gotTruncated)
		} else {
			assert.Equal(t, s.wantTruncated, gotTruncated, "truncation")
		}
		assert.Equal(t, s.wantInvariant, invariant)
	}
}

func TestAsTime(t *testing.T) {
	stdlib := time.Date(1986, time.October, 1, 0, 0, 0, 0, time.UTC)

	const rate = 500
	tm := New[uint64](stdlib.Unix(), rate)
	if got, want := tm.AsTime(), stdlib; !got.Equal(want) {
		t.Fatalf("%T.AsTime() at construction got %v; want %v", tm, got, want)
	}

	tm.Tick(1)
	if got, want := tm.AsTime(), stdlib.Add(2*time.Millisecond); !got.Equal(want) {
		t.Fatalf("%T.AsTime() after ticking 1/%d got %v; want %v", tm, rate, got, want)
	}
}

func TestCanotoRoundTrip(t *testing.T) {
	const (
		seconds = 42
		rate    = 10_000
		tick    = 1_234
	)
	tm := New[uint64](seconds, rate)
	tm.Tick(tick)

	got := new(Time[uint64])
	require.NoErrorf(t, got.UnmarshalCanoto(tm.MarshalCanoto()), "%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", got)
	got.assertEq(t, fmt.Sprintf("%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", tm), seconds, frac(tick, rate))
}

func TestFastForward(t *testing.T) {
	tm := New(42, uint64(1000))

	steps := []struct {
		tickBefore uint64
		ffTo       int64
		wantSec    int64
		wantFrac   FractionalSecond[uint64]
	}{
		{100, 42, 0, frac(0, 1000)},
		{0, 43, 0, frac(900, 1000)},
		{0, 44, 1, frac(0, 1000)},
		{200, 50, 5, frac(800, 1000)},
	}

	for _, s := range steps {
		tm.Tick(s.tickBefore)
		gotSec, gotFrac := tm.FastForwardTo(s.ffTo)
		assert.Equal(t, s.wantSec, gotSec)
		assert.Equal(t, s.wantFrac, gotFrac)

		if t.Failed() {
			break
		}
	}
}

func TestCmpUnix(t *testing.T) {
	tests := []struct {
		tm         *Time[uint64]
		tick       uint64
		cmpAgainst int64
		want       int
	}{
		{
			tm:         New[uint64](42, 1e6),
			cmpAgainst: 42,
			want:       0,
		},
		{
			tm:         New[uint64](42, 1e6),
			tick:       1,
			cmpAgainst: 42,
			want:       1,
		},
		{
			tm:         New[uint64](41, 100),
			tick:       99,
			cmpAgainst: 42,
			want:       -1,
		},
	}

	for _, tt := range tests {
		tt.tm.Tick(tt.tick)
		if got := tt.tm.CmpUnix(tt.cmpAgainst); got != tt.want {
			t.Errorf("Time{%d + %d/%d}.CmpUnix(%d) got %d; want %d", tt.tm.Unix(), tt.tm.fraction, tt.tm.hertz, tt.cmpAgainst, got, tt.want)
		}
	}
}
