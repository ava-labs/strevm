// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"net/http"
)

// CreateHandlers returns all VM-specific HTTP handlers to be exposed by the
// node, keyed by path.
func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return nil, errUnimplemented
}

// CreateHTTP2Handler returns `(nil, nil)`.
func (vm *VM) CreateHTTP2Handler(ctx context.Context) (http.Handler, error) {
	return nil, nil
}
