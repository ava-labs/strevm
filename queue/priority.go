package queue

import "container/heap"

// A LessThan implementation has a strict ordering.
type LessThan[T any] interface {
	LessThan(T) bool
}

// A Priority is a priority queue. The zero value is valid. It wraps a
// [heap.Interface] and exposes methods with the same semantics and complexity
// as the [heap] package's functions.
type Priority[T LessThan[T]] struct {
	p priority[T]
}

// Len returns the number of items in the queue.
func (p *Priority[T]) Len() int {
	return p.p.Len()
}

// Push adds an item to the queue.
func (p *Priority[T]) Push(x T) {
	heap.Push(&p.p, x)
}

// Peek returns the first value in the queue without removing it. It panics if
// the queue is empty.
func (p *Priority[T]) Peek() T {
	return p.p.peekFront()
}

// Pop removes and returns the first element from the queue. It panics if the
// queue is empty.
func (p *Priority[T]) Pop() T {
	return heap.Pop(&p.p).(T)
}

// Fix reestablishes the queue's ordering if the i'th element's priority
// changes.
func (p *Priority[T]) Fix(i int) {
	heap.Fix(&p.p, i)
}

// Grow increase's the queue's allocated buffer to hold up to `n` items. This
// does not place a limit on the size of the queue, but pre-allocates memory.
func (p *Priority[T]) Grow(n int) {
	p.p.grow(n)
}

// priority implements [heap.Interface].
type priority[T LessThan[T]] struct {
	list[T]
}

func (p *priority[T]) Len() int {
	return p.len()
}

func (p *priority[T]) Less(i, j int) bool {
	return p.peekAt(i).LessThan(p.peekAt(j))
}

func (p *priority[T]) Pop() any {
	return p.popBack()
}

func (p *priority[T]) Push(x any) {
	p.append(x.(T))
}

func (p *priority[T]) Swap(i, j int) {
	i = p.ringIndex(i)
	j = p.ringIndex(j)
	p.ring[i], p.ring[j] = p.ring[j], p.ring[i]
}
