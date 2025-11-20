// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package sae implements the [Streaming Asynchronous Execution] (SAE) virtual
// machine to be compatible with Avalanche consensus.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/snow"
)

// VM implements all of [adaptor.ChainVM] except for the `Initialize` method,
// which needs to be provided by a harness. In all cases, the harness MUST
// provide a last-synchronous block, which MAY be the genesis.
type VM struct{}

// SetState notifies the VM of a transition in the state lifecycle.
func (vm *VM) SetState(ctx context.Context, state snow.State) error {
	return errUnimplemented
}

// Shutdown gracefully closes the VM.
func (vm *VM) Shutdown(context.Context) error {
	return errUnimplemented
}

// Version reports the VM's version.
func (vm *VM) Version(context.Context) (string, error) {
	return "", errUnimplemented
}
