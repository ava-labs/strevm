// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package queue

// A list is a slice-like data structure with constant-time popping and
// amoritised constant-time pushing.
type list[T any] struct {
	ring  []T // len(ring) MUST == cap(ring)
	start int // 0 <= start < len(ring)
	n     int // 0 <= start <= len(ring)
}

// cap returns the capacity of the list.
func (l *list[T]) cap() int {
	return len(l.ring)
}

// len returns the number of elements in the list.
func (l *list[T]) len() int {
	return l.n
}

func (l *list[T]) mod(i int) int {
	return i % l.cap()
}

func (l *list[T]) ringIndex(i int) int {
	return l.mod(l.start + i)
}

// nextRingIndex returns the index in the list's ring at which a new element
// should be inserted when appending to the list. If the list is at capacity, it
// always returns (0, false), and [list.grow] should be called.
func (l *list[T]) nextRingIndex() (_ int, hasCap bool) {
	if l.len() == l.cap() {
		return 0, false
	}
	return l.ringIndex(l.n), true
}

// append appends `x` to the end of the list. If the list is full, its capacity
// is doubled, or set to 1 if there is no current capacity.
func (l *list[T]) append(x T) {
	for {
		if idx, hasSpace := l.nextRingIndex(); hasSpace {
			l.ring[idx] = x
			l.n++
			return
		}

		grow := 2 * l.cap()
		if grow == 0 {
			grow = 1
		}
		l.grow(grow)
	}
}

// peekFront returns the first element in the list without removing it. It
// panics if the list is empty.
func (l *list[T]) peekFront() T {
	if l.n == 0 {
		panic("peek into empty list")
	}
	return l.ring[l.start]
}

// peekAt returns the i'th element in the list without removing it. Unlike
// [list.peekFront], it never panics but the returned value is undefined if `i`
// is not in `[0,l.len())`
func (l *list[T]) peekAt(i int) T {
	return l.ring[l.ringIndex(i)]
}

func (l *list[T]) clearAt(i int) {
	var zero T
	l.ring[l.ringIndex(i)] = zero
}

// popFront removes and returns the first element from the list. It panics if
// the list is empty.
func (l *list[T]) popFront() T {
	if l.n == 0 {
		panic("pop from front of empty list")
	}

	x := l.peekAt(0)
	l.clearAt(0)
	l.start = l.mod(l.start + 1)
	l.n--
	return x
}

// popBack removes and returns the last element from the list. It panics if the
// list is empty.
func (l *list[T]) popBack() T {
	if l.n == 0 {
		panic("pop from back of empty list")
	}

	index := l.n - 1
	x := l.peekAt(index)
	l.clearAt(index)
	l.n--
	return x
}

// grow increases the list's capacity to n, if necessary. It is O(l.cap()).
func (l *list[T]) grow(n int) {
	if n <= l.cap() {
		return
	}
	b := make([]T, n)
	copy(b, l.ring[l.start:])
	copy(b[len(l.ring)-l.start:], l.ring[:l.start])

	l.ring = b
	l.start = 0
}
