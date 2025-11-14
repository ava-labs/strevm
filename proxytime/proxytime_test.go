// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proxytime

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gocmp "github.com/google/go-cmp/cmp"
)

func frac(num, den uint64) FractionalSecond[uint64] {
	return FractionalSecond[uint64]{Numerator: num, Denominator: den}
}

func (tm *Time[D]) assertEq(tb testing.TB, desc string, seconds uint64, fraction FractionalSecond[D]) (equal bool) {
	tb.Helper()
	want := &Time[D]{
		seconds:  seconds,
		fraction: fraction.Numerator,
		hertz:    fraction.Denominator,
	}
	if diff := gocmp.Diff(want, tm, CmpOpt[D](IgnoreRateInvariants)); diff != "" {
		tb.Errorf("%s diff (-want +got):\n%s", desc, diff)
		return false
	}
	return true
}

func (tm *Time[D]) requireEq(tb testing.TB, desc string, seconds uint64, fraction FractionalSecond[D]) {
	tb.Helper()
	if !tm.assertEq(tb, desc, seconds, fraction) {
		tb.FailNow()
	}
}

func TestTickAndCmp(t *testing.T) {
	const rate = 500
	tm := New(0, uint64(500))
	tm.assertEq(t, "New(0, ...)", 0, frac(0, rate))

	steps := []struct {
		tick              uint64
		wantSec, wantFrac uint64
	}{
		{
			tick:    100,
			wantSec: 0, wantFrac: 100,
		},
		{
			tick:    399,
			wantSec: 0, wantFrac: 499,
		},
		{
			// Although this is a no-op, it's useful to see the fraction for
			// understanding the next step.
			tick:    0,
			wantSec: 0, wantFrac: rate - 1,
		},
		{
			tick:    1,
			wantSec: 1, wantFrac: 0,
		},
		{
			tick:    rate,
			wantSec: 2, wantFrac: 0,
		},
		{
			tick:    400,
			wantSec: 2, wantFrac: 400,
		},
		{
			tick:    200,
			wantSec: 3, wantFrac: 100,
		},
		{
			tick:    3*rate + 100,
			wantSec: 6, wantFrac: 200,
		},
		{
			tick:    299,
			wantSec: 6, wantFrac: 499,
		},
		{
			tick:    2,
			wantSec: 7, wantFrac: 1,
		},
		{
			tick:    rate - 1,
			wantSec: 8, wantFrac: 0,
		},
		{
			// Set fraction to anything non-zero so we can test overflow
			// prevention with a tick of 2^64-1.
			tick:    1,
			wantSec: 8, wantFrac: 1,
		},
		{
			tick:     math.MaxUint64,
			wantSec:  8 + math.MaxUint64/rate,
			wantFrac: 1 + math.MaxUint64%rate,
		},
	}

	var ticked uint64
	for _, s := range steps {
		old := tm.Clone()

		tm.Tick(s.tick)
		ticked += s.tick
		tm.requireEq(t, fmt.Sprintf("%+d", ticked), s.wantSec, frac(s.wantFrac, rate))

		if got, want := tm.Compare(old), cmp.Compare(s.tick, 0); got != want {
			t.Errorf("After %T.Tick(%d); ticked.Cmp(original) got %d; want %d", tm, s.tick, got, want)
		}
		if got, want := old.Compare(tm), cmp.Compare(0, s.tick); got != want {
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
		newRate, wantNumerator uint64
		wantTruncated          FractionalSecond[uint64]
		wantInvariant          uint64
	}{
		{
			newRate:       initRate / divisor, // no rounding
			wantNumerator: tick / divisor,
			wantTruncated: frac(0, 1),
			wantInvariant: invariant / divisor,
		},
		{
			newRate:       initRate * 5,
			wantNumerator: tick * 5,
			wantTruncated: frac(0, 1), // multiplication never has rounding
			wantInvariant: invariant * 5,
		},
		{
			newRate:       15_000, // same as above, but shows the numbers explicitly
			wantNumerator: 1_500,
			wantTruncated: frac(0, 1),
			wantInvariant: 3_000,
		},
		{
			newRate:       75,
			wantNumerator: 7,                   // 7.5
			wantTruncated: frac(7_500, 15_000), // rounded down by 0.5, denominated in the old rate
			wantInvariant: 15,
		},
	}

	for _, s := range steps {
		old := tm.Rate()
		gotTruncated, err := tm.SetRate(s.newRate)
		require.NoErrorf(t, err, "%T.SetRate(%d)", tm, s.newRate)
		desc := fmt.Sprintf("rate changed from %d to %d", old, s.newRate)
		tm.requireEq(t, desc, initSeconds, frac(s.wantNumerator, s.newRate))

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

	const rate uint64 = 500
	tm := New(uint64(stdlib.Unix()), rate) //nolint:gosec // Known to not overflow
	if got, want := tm.AsTime(), stdlib; !got.Equal(want) {
		t.Fatalf("%T.AsTime() at construction got %v; want %v", tm, got, want)
	}

	tm.Tick(1)
	if got, want := tm.AsTime(), stdlib.Add(2*time.Millisecond); !got.Equal(want) {
		t.Fatalf("%T.AsTime() after ticking 1/%d got %v; want %v", tm, rate, got, want)
	}
}

func TestCanotoRoundTrip(t *testing.T) {
	tests := []struct {
		name                string
		seconds, rate, tick uint64
	}{
		{
			name:    "non_zero_fields",
			seconds: 42,
			rate:    10_000,
			tick:    1_234,
		},
		{
			name: "zero_seconds",
			rate: 100,
			tick: 1,
		},
		{
			name:    "zero_fractional_second",
			seconds: 999,
			rate:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := New(tt.seconds, tt.rate)
			tm.Tick(tt.tick)

			got := new(Time[uint64])
			require.NoErrorf(t, got.UnmarshalCanoto(tm.MarshalCanoto()), "%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", got)
			got.assertEq(t, fmt.Sprintf("%T.UnmarshalCanoto(%[1]T.MarshalCanoto())", tm), tt.seconds, frac(tt.tick, tt.rate))
		})
	}
}

func TestFastForward(t *testing.T) {
	const rate = uint64(1000)
	tm := New(42, rate)

	steps := []struct {
		tickBefore uint64
		ffTo       uint64
		ffToFrac   uint64
		wantSec    uint64
		wantFrac   FractionalSecond[uint64]
	}{
		{
			tickBefore: 0,
			ffTo:       41, // in the past
			wantSec:    0,
			wantFrac:   frac(0, rate),
		},
		{
			tickBefore: 100, // 42.100
			ffTo:       42,  // in the past
			wantSec:    0,
			wantFrac:   frac(0, rate),
		},
		{
			tickBefore: 0, // 42.100
			ffTo:       43,
			wantSec:    0,
			wantFrac:   frac(900, rate),
		},
		{
			tickBefore: 0, // 43.000
			ffTo:       44,
			wantSec:    1,
			wantFrac:   frac(0, rate),
		},
		{
			tickBefore: 200, // 44.200
			ffTo:       50,
			wantSec:    5,
			wantFrac:   frac(800, rate),
		},
		{
			tickBefore: 0, // 50.000
			ffTo:       50,
			ffToFrac:   900,
			wantSec:    0,
			wantFrac:   frac(900, rate),
		},
		{
			tickBefore: 0, // 50.900
			ffTo:       51,
			ffToFrac:   100,
			wantSec:    0,
			wantFrac:   frac(200, rate),
		},
		{
			tickBefore: 100, // 51.200
			ffTo:       51,
			ffToFrac:   200,
			wantSec:    0,
			wantFrac:   frac(0, rate),
		},
	}

	for _, s := range steps {
		tm.Tick(s.tickBefore)
		gotSec, gotFrac := tm.FastForwardTo(s.ffTo, s.ffToFrac)
		assert.Equal(t, s.wantSec, gotSec, "Fast-forwarded seconds")
		assert.Equal(t, s.wantFrac, gotFrac, "Fast-forwarded fractional numerator")

		if t.Failed() {
			t.FailNow()
		}
	}
}

func TestConvertMilliseconds(t *testing.T) {
	tests := []struct {
		rate    uint64
		ms      uint16
		want    uint64
		wantErr error
	}{
		{
			rate: 1000,
			ms:   42,
			want: 42,
		},
		{
			rate: 1234 * 2,
			ms:   1000 / 2,
			want: 1234,
		},
		{
			rate: 98765 * 4,
			ms:   1000 / 4,
			want: 98765,
		},
		{
			rate: 142857 * 1000,
			ms:   1,
			want: 142857,
		},
		{
			rate: 314159,
			ms:   1000,
			want: 314159,
		},
		{
			rate:    math.MaxUint64, // arbitrary
			ms:      1001,
			wantErr: errGtSecond,
		},
		{
			rate: 1_001,
			ms:   500,
			want: 500,
		},
		{
			rate: 1_001,
			ms:   1000,
			want: 1_001,
		},
	}

	for _, tt := range tests {
		got, err := New(0, tt.rate).ConvertMilliseconds(tt.ms)
		want := FractionalSecond[uint64]{
			Numerator:   tt.want,
			Denominator: tt.rate,
		}
		if got != want || !errors.Is(err, tt.wantErr) {
			t.Errorf("New(0, %d).ConvertMilliseconds(%d) got (%v, %v); want (%v, %v)", tt.rate, tt.ms, got, err, want, tt.wantErr)
		}
	}
}

func TestCmpUnix(t *testing.T) {
	tests := []struct {
		tm         *Time[uint64]
		tick       uint64
		cmpAgainst uint64
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
		if got := tt.tm.CompareUnix(tt.cmpAgainst); got != tt.want {
			t.Errorf("Time{%d + %d/%d}.CmpUnix(%d) got %d; want %d", tt.tm.Unix(), tt.tm.fraction, tt.tm.hertz, tt.cmpAgainst, got, tt.want)
		}
	}
}
