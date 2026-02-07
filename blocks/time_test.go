// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/proxytime"
)

func gasExtractionCmpOpt() cmp.Option {
	return proxytime.CmpOpt[gas.Gas](proxytime.IgnoreRateInvariants)
}

func TestGasTime(t *testing.T) {
	const (
		unix     = 42
		subSecMs = 500
		target   = 500
		millis   = subSecMs * int64(time.Millisecond)
		// frac = MulDivCeil(millis, rate, time.Second)
		// With rate=500 * 2 = 1000: ceil(500_000_000 * 1000 / 1_000_000_000) = 500
		frac = 500
	)
	rate := gastime.SafeRateOfTarget(target)

	hooks := &hookstest.Stub{
		Now: func() time.Time {
			return time.Unix(unix, millis)
		},
		Target: target,
	}
	parent := &types.Header{
		Number: big.NewInt(1),
	}
	hdr := hooks.BuildHeader(parent)

	got := GasTime(hooks, hdr, parent)
	want := proxytime.New(unix, rate)
	want.Tick(frac)

	if diff := cmp.Diff(want, got, gasExtractionCmpOpt()); diff != "" {
		t.Errorf("GasTime(...) diff (-want +got):\n%s", diff)
	}
}

func TestPreciseTime_PreACP226Fallback(t *testing.T) {
	hooks := &hookstest.Stub{}
	hdr := &types.Header{
		Time: 42,
	}

	got := PreciseTime(hooks, hdr)
	want := time.UnixMilli(42_000)
	require.Equal(t, want, got)
}

func FuzzTimeExtraction(f *testing.F) {
	// There are two different ways that the gas time of a block can be
	// calculated, both of which result in the same value. While neither can
	// result in an overflow due to required invariants, this guarantee is much
	// easier to reason about when inspecting [GasTime]. The alternative, via
	// [proxytime.Of] and then [proxytime.Time.SetRate], is more obviously
	// correct but too general-purpose and requires ignoring/checking a returned
	// `error`. We can therefore use this equivalence for differential fuzzing.
	//
	// ACP-226 provides millisecond precision, so the fuzz input is in
	// milliseconds (0-999) rather than arbitrary nanoseconds.

	f.Fuzz(func(t *testing.T, unix int64, target uint64, subSecMs int64) {
		if unix < 0 {
			t.Skip("ACP-226 timestamps are non-negative")
		}
		if subSecMs < 0 || subSecMs >= 1000 {
			t.Skip("Invalid sub-second millisecond value")
		}
		if target == 0 {
			t.Skip("Zero target")
		}

		subSec := subSecMs * int64(time.Millisecond)
		hooks := &hookstest.Stub{
			Now: func() time.Time {
				return time.Unix(unix, subSec)
			},
			Target: gas.Gas(target),
		}
		parent := &types.Header{
			Number: big.NewInt(1),
		}
		hdr := hooks.BuildHeader(parent)

		t.Run("PreciseTime", func(t *testing.T) {
			got := PreciseTime(hooks, hdr)
			want := hooks.Now()
			if got != want {
				t.Errorf("PreciseTime() = %v; want %v", got, want)
			}
		})

		t.Run("GasTime", func(t *testing.T) {
			got := GasTime(hooks, hdr, parent)

			want := proxytime.Of[gas.Gas](hooks.Now())
			rate := gastime.SafeRateOfTarget(gas.Gas(target))
			require.NoErrorf(t, want.SetRate(rate), "%T.SetRate(%d)", want, rate)

			if diff := cmp.Diff(want, got, gasExtractionCmpOpt()); diff != "" {
				t.Errorf("diff (-proxytime.Of +GasTime):\n%s", diff)
			}
		})
	})
}
