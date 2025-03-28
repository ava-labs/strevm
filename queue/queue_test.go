package queue

import (
	"math/rand/v2"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func all[T comparable](t *testing.T, q *FIFO[T]) []T {
	t.Helper()

	var got []T
	for {
		peek, peekOK := q.Peek()
		pop, popOK := q.Pop()
		if peek != pop || peekOK != popOK {
			t.Errorf("{%T.Peek() = (%v, %t)} != {Pop() = (%v, %t)}", q, peek, peekOK, pop, popOK)
		}
		if !popOK {
			break
		}
		got = append(got, pop)
	}
	return got
}

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

		rng := rand.New(rand.NewPCG(0, 0))

		var got, want []int
		for i := range 1000 {
			q.Push(i)
			want = append(want, i)

			if rng.IntN(4) == 0 {
				x, ok := q.Pop()
				if ok {
					got = append(got, x)
				}
			}
		}

		got = append(got, all(t, &q)...)
		diff(t, got, want)
	})
}
