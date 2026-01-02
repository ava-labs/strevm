// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/version"
)

// Connected notifies the VM that a p2p connection has been established with the
// specified node.
func (vm *VM) Connected(
	ctx context.Context,
	nodeID ids.NodeID,
	nodeVersion *version.Application,
) error {
	return errUnimplemented
}

// Disconnected notifies the VM that the p2p connection with the specified node
// has terminated.
func (vm *VM) Disconnected(ctx context.Context, nodeID ids.NodeID) error {
	return errUnimplemented
}

// AppRequest notifies the VM of an incoming request from the specified node.
func (vm *VM) AppRequest(
	ctx context.Context,
	nodeID ids.NodeID,
	requestID uint32,
	deadline time.Time,
	request []byte,
) error {
	return errUnimplemented
}

// AppResponse notifies the VM of an incoming response from the specified node.
func (vm *VM) AppResponse(
	ctx context.Context,
	nodeID ids.NodeID,
	requestID uint32,
	response []byte,
) error {
	return errUnimplemented
}

// AppRequestFailed notifies the VM that an outgoing request failed.
func (vm *VM) AppRequestFailed(
	ctx context.Context,
	nodeID ids.NodeID,
	requestID uint32,
	appErr *snowcommon.AppError,
) error {
	return errUnimplemented
}

// AppGossip notifies the VM of gossip from the specified node.
func (vm *VM) AppGossip(
	ctx context.Context,
	nodeID ids.NodeID,
	msg []byte,
) error {
	return errUnimplemented
}
