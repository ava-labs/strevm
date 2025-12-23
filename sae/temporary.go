// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// This file MUST be deleted before release. It is intended solely to house
// interim identifiers needed for development over multiple PRs.

import (
	"context"
	"errors"

	"github.com/ava-labs/libevm/core/types"
)

var errUnimplemented = errors.New("unimplemented")

// TODO(arr4n) remove this method once no longer embedding [ethapi.Backend] in
// [ethAPIBackend] as it's only required for disambiguation.
func (b *ethAPIBackend) SendTx(ctx context.Context, tx *types.Transaction) error {
	return b.Set.SendTx(ctx, tx)
}
