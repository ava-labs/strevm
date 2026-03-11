// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpc

func (b *apiBackend) SetHead(uint64) {
	b.vm.Logger().Info("debug_setHead called but not supported by SAE")
}
