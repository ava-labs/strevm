package sae

import (
	"errors"

	"golang.org/x/exp/constraints"
)

const (
	maxGasPerChunk        = 10_000_000
	stateRootDelaySeconds = 5
)

var errUnimplemented = errors.New("unimplemented")

func send[Q ~struct{}, T any](quit <-chan Q, ch chan<- T, v T) bool {
	select {
	case <-quit:
		return false
	case ch <- v:
		return true
	}
}

func zero[T any]() (z T) { return }

func recv[Q ~struct{}, T any](quit <-chan Q, ch <-chan T) (T, bool) {
	select {
	case <-quit:
		return zero[T](), false
	case v, ok := <-ch:
		if !ok {
			panic("channel closed unexpectedly")
		}
		return v, true
	}
}

// clippedSubtract returns max(0,a-b) without underflow.
func clippedSubtract[T constraints.Unsigned](a, b T) T {
	if b >= a {
		return 0
	}
	return a - b
}
