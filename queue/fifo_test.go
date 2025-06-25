// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package queue

import (
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestFIFO(t *testing.T) {
	diff := func(t *testing.T, got, want []int) {
		t.Helper()
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%T.Pop() until !ok; diff (-want +got):\n%s", FIFO[int]{}, diff)
		}
	}

	t.Run("disjoint_Push_Pop", func(t *testing.T) {
		var q FIFO[int]

		var want []int
		for i := range 5 {
			q.Push(i)
			want = append(want, i)
		}
		diff(t, all(t, &q), want)
	})

	t.Run("interleaved_Push_Pop", func(t *testing.T) {
		var q FIFO[int]

		rng := rand.New(rand.NewPCG(0, 0)) //nolint:gosec // Reproducibility is useful in tests

		var got, want []int
		for i := range 1000 {
			q.Push(i)
			want = append(want, i)

			if rng.IntN(4) == 0 && q.Len() > 0 {
				got = append(got, q.Pop())
			}
		}

		got = append(got, all(t, &q)...)
		diff(t, got, want)
	})
}
