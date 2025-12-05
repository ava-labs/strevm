// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"net/http"
)

// CreateHandlers returns all VM-specific HTTP handlers to be exposed by the
// node, keyed by extension.
func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return nil, errUnimplemented
}

// NewHTTPHandler returns the HTTP handler that will be invoked if a client
// passes this VM's chain ID via the routing header described in the [common.VM]
// documentation for this method.
func (vm *VM) NewHTTPHandler(context.Context) (http.Handler, error) {
	return nil, errUnimplemented
}
