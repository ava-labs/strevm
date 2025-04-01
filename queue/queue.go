package queue

// A FIFO is an unbounded first-in-first-out queue. The zero value is valid.
type FIFO[T any] struct {
	list[T]
}

// Len returns the number of items in the queue.
func (f *FIFO[T]) Len() int {
	return f.len()
}

// Push appends an item to the queue.
func (f *FIFO[T]) Push(x T) {
	f.append(x)
}

// Peek returns the first value in the queue without removing it. It panics if
// the queue is empty.
func (f *FIFO[T]) Peek() T {
	return f.peekFront()
}

// Pop removes and returns the first element from the queue. It panics if the
// queue is empty.
func (f *FIFO[T]) Pop() T {
	return f.popFront()
}

// Grow increase's the queue's allocated buffer to hold up to `n` items. This
// does not place a limit on the size of the queue, but pre-allocates memory.
func (f *FIFO[T]) Grow(n int) {
	f.grow(n)
}
