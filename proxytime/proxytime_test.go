package proxytime

import (
	"cmp"
	"fmt"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func frac(num, den uint64) FractionalSecond[uint64] {
	return FractionalSecond[uint64]{Numerator: num, Denominator: den}
}

func (tm *Time[D]) assertEq(tb testing.TB, desc string, seconds uint64, fraction FractionalSecond[D]) {
	tb.Helper()
	if tm.Unix() != seconds || tm.Fraction() != fraction {
		tb.Errorf("%s got (seconds, fraction) = (%d, %d); want (%d, %d)", desc, tm.seconds, tm.fraction, seconds, fraction)
	}
}

func (tm *Time[D]) requireEq(tb testing.TB, desc string, seconds uint64, fraction FractionalSecond[D]) {
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
		tick                      uint64
		wantSeconds, wantFraction uint64
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
		old := tm.Copy()

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
	require.Equalf(t, initRate, tm.Rate(), "%T.Rate()")

	steps := []struct {
		newRate, wantFraction uint64
		wantTruncated         FractionalSecond[uint64]
	}{
		{initRate / divisor, tick / divisor, frac(0, 1)}, // no rounding
		{initRate * 5, tick * 5, frac(0, 1)},             // multiplication never has rounding
		{15_000, 1_500, frac(0, 1)},                      // same as above, but shows the numbers
		{75, 7 /*7.5*/, frac(7_500, 15_000)},             // rounded down by half, denominated in the old rate
	}

	for _, s := range steps {
		old := tm.Rate()
		gotTruncated := tm.SetRate(s.newRate)
		tm.requireEq(t, fmt.Sprintf("rate changed from %d to %d", old, s.newRate), initSeconds, frac(s.wantFraction, s.newRate))
		require.Equalf(t, s.newRate, tm.Rate(), "%T.Rate() after %[1]T.SetRate(%2)", tm, s.newRate)

		if gotTruncated.Numerator == 0 && s.wantTruncated.Numerator == 0 {
			assert.NotZerof(t, gotTruncated.Denominator, "%T.Denominator")
		} else {
			assert.Equalf(t, s.wantTruncated, gotTruncated, "")
		}
	}
}

func TestAsTime(t *testing.T) {
	stdlib := time.Date(1986, time.October, 1, 0, 0, 0, 0, time.UTC)

	const rate = 500
	tm := New[uint64](uint64(stdlib.Unix()), rate)
	if diff := gocmp.Diff(stdlib, tm.AsTime()); diff != "" {
		t.Fatalf("%T.AsTime() at construction (-want +got):\n%s", tm, diff)
	}

	tm.Tick(1)
	if diff := gocmp.Diff(stdlib.Add(2*time.Millisecond), tm.AsTime()); diff != "" {
		t.Fatalf("%T.AsTime() after ticking 1/%d (-want +got)\n%s", tm, rate, diff)
	}
}

func TestParseBytes(t *testing.T) {
	const (
		seconds = 42
		rate    = 10_000
		tick    = 1_234
	)
	tm := New[uint64](seconds, rate)
	tm.Tick(tick)

	got, err := Parse[uint64](tm.Bytes())
	require.NoError(t, err, "Parse(New(...))")
	got.assertEq(t, fmt.Sprintf("Parse(%T.Bytes())", tm), seconds, frac(tick, rate))
}
