// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package queue provides typed queue implementations with a common interface.
package queue

// Queue is the common interface shared by implementations.
type Queue[T any] interface {
	Len() int
	Push(T)
	Peek() T
	Pop() T
	Grow(int)
}
