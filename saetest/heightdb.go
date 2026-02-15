// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saetest

import (
	"maps"
	"slices"
	"sync"

	"github.com/ava-labs/avalanchego/database"

	"github.com/ava-labs/strevm/saedb"
)

// A ClonableHeightIndex extends [database.HeightIndex] with the ability to
// clone itself.
type ClonableHeightIndex interface {
	database.HeightIndex
	Clone() ClonableHeightIndex
}

// NewHeightIndexDB returns an in-memory [database.HeightIndex]; its additional
// `Clone()` method can be called before or after closing, and the clone will
// not be closed in either circumstance.
func NewHeightIndexDB() ClonableHeightIndex {
	return &hIndex{
		data: make(map[uint64][]byte),
	}
}

// NewExecutionResultsDB wraps and returns a [NewHeightIndexDB].
func NewExecutionResultsDB() saedb.ExecutionResults {
	return saedb.ExecutionResults{HeightIndex: NewHeightIndexDB()}
}

type hIndex struct {
	mu     sync.RWMutex
	data   map[uint64][]byte
	closed bool
}

func readHIndex[T any](h *hIndex, fn func() (T, error)) (T, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		var zero T
		return zero, database.ErrClosed
	}
	return fn()
}

func (h *hIndex) write(fn func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return database.ErrClosed
	}
	return fn()
}

func (h *hIndex) Put(n uint64, b []byte) error {
	return h.write(func() error {
		h.data[n] = slices.Clone(b)
		return nil
	})
}

func (h *hIndex) Get(n uint64) ([]byte, error) {
	return readHIndex(h, func() ([]byte, error) {
		b, ok := h.data[n]
		if !ok {
			return nil, database.ErrNotFound
		}
		return slices.Clone(b), nil
	})
}

func (h *hIndex) Clone() ClonableHeightIndex {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return &hIndex{
		data: maps.Clone(h.data),
	}
}

func (h *hIndex) Has(n uint64) (bool, error) {
	return readHIndex(h, func() (bool, error) {
		_, ok := h.data[n]
		return ok, nil
	})
}

func (h *hIndex) Sync(_, _ uint64) error {
	return h.write(func() error { return nil })
}

func (h *hIndex) Close() error {
	return h.write(func() error {
		h.closed = true
		return nil
	})
}
