// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// This file MUST be deleted before release. It is intended solely to house
// interim identifiers needed for development over multiple PRs.

import (
	"context"
	"errors"

	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
)

var errUnimplemented = errors.New("unimplemented")

// TODO(arr4n) remove these methods once no longer embedding [ethapi.Backend] in
// [ethAPIBackend] as they're only required for disambiguation.

func (b *ethAPIBackend) SendTx(ctx context.Context, tx *types.Transaction) error {
	return b.Set.SendTx(ctx, tx)
}

func (b *ethAPIBackend) CurrentHeader() *types.Header {
	return b.chainIndexer.CurrentHeader()
}

func (b *ethAPIBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.chainIndexer.SubscribeChainHeadEvent(ch)
}

func (b *ethAPIBackend) BloomStatus() (uint64, uint64) {
	return b.bloomIndexer.BloomStatus()
}

func (b *ethAPIBackend) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	b.bloomIndexer.ServiceFilter(ctx, session)
}
