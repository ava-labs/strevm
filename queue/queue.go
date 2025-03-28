package queue

type FIFO[T any] struct {
	ring  []T // len(ring) MUST == cap(ring)
	start int // 0 <= start < len(ring)
	n     int // 0 <= start <= len(ring)
}

func (f *FIFO[T]) cap() int {
	return len(f.ring)
}

func (f *FIFO[T]) Len() int {
	return f.n
}

func (f *FIFO[T]) mod(i int) int {
	return i % f.cap()
}

func (f *FIFO[T]) nextIndex() (_ int, hasSpace bool) {
	if f.Len() == f.cap() {
		return 0, false
	}
	return f.mod(f.start + f.n), true
}

func (f *FIFO[T]) Push(x T) {
	for {
		if idx, hasSpace := f.nextIndex(); hasSpace {
			f.ring[idx] = x
			f.n++
			return
		}

		grow := 2 * f.cap()
		if grow == 0 {
			grow = 1
		}
		f.Grow(grow)
	}
}

func zero[T any]() (z T) { return }

func (f *FIFO[T]) Peek() (T, bool) {
	if f.n == 0 {
		return zero[T](), false
	}
	return f.ring[f.start], true
}

func (f *FIFO[T]) Pop() (T, bool) {
	if f.n == 0 {
		return zero[T](), false
	}

	x := f.ring[f.start]
	f.start = f.mod(f.start + 1)
	f.n--
	return x, true
}

func (f *FIFO[T]) Grow(n int) {
	if n <= f.cap() {
		return
	}
	b := make([]T, n)
	copy(b, f.ring[f.start:])
	copy(b[len(f.ring)-f.start:], f.ring[:f.start])

	f.ring = b
	f.start = 0
}
