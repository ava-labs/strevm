//go:build !dev

package sae

import (
	"context"

	"github.com/ava-labs/avalanchego/snow/engine/common"
)

// afterInitialize is a no-op in a non-dev-tagged build.
func (vm *VM) afterInitialize(context.Context, chan<- common.Message) error { return nil }
