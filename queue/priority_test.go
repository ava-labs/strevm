package queue

import (
	"math/rand/v2"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
)

type Int int

func (i Int) LessThan(j Int) bool {
	return i < Int(j)
}

func TestPriority(t *testing.T) {
	p := new(Priority[Int])

	rng := rand.New(rand.NewPCG(0, 0))
	var want []Int
	for range 32 {
		i := Int(rng.IntN(100))
		p.Push(i)
		want = append(want, i)
	}
	sort.Slice(want, func(i, j int) bool {
		return want[i].LessThan(want[j])
	})

	if diff := cmp.Diff(want, all(t, p)); diff != "" {
		t.Error(diff)
	}
}
