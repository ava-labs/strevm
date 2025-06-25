// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package queue

import (
	"testing"
)

func all[T comparable, Q Queue[T]](t *testing.T, q Q) []T {
	t.Helper()

	var got []T
	for q.Len() > 0 {
		peek := q.Peek()
		pop := q.Pop()
		if peek != pop {
			t.Errorf("{%T.Peek() = %v} != {Pop() = %v}", q, peek, pop)
		}
		got = append(got, pop)
	}
	return got
}

func TestClearPointersOnPop(t *testing.T) {
	var q FIFO[*int]
	for i := range 16 {
		q.Push(&i)
	}
	for q.Len() > 0 {
		q.Pop()
	}
	for _, x := range q.ring {
		if x != nil {
			t.Fatalf("%T.Pop() did not set underlying pointer to nil", q)
		}
	}
}
