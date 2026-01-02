// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"

	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/blocks"
)

var _ adaptor.ChainVM[*blocks.Block] = (*SinceGenesis)(nil)

// SinceGenesis is a harness around a [VM], providing an `Initialize` method
// that treats the chain as being asynchronous since genesis.
type SinceGenesis struct {
	*VM
}

// Initialize initializes the VM.
func (vm *SinceGenesis) Initialize(
	ctx context.Context,
	chainCtx *snow.Context,
	db database.Database,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	fxs []*snowcommon.Fx,
	appSender snowcommon.AppSender,
) error {
	// TODO(arr4n) when implementing this, also consolidate [NewVM] and
	// [VM.Init] so a freshly constructed [VM] is ready to use.
	return errUnimplemented
}
