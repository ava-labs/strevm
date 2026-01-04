// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"

	"github.com/ava-labs/strevm/blocks"
)

var _ core.ChainContext = (*chainContext)(nil)

type chainContext struct {
	blocks blocks.Source
	log    logging.Logger
}

func (c *chainContext) GetHeader(h common.Hash, n uint64) *types.Header {
	return c.blocks.Header(h, n)
}

func (c *chainContext) Engine() consensus.Engine {
	// This is serious enough that it needs to be investigated immediately, but
	// not enough to be fatal. It will also cause tests to fail if ever called,
	// so we can catch it early.
	c.log.Error("ChainContext.Engine() called unexpectedly")
	return nil
}
