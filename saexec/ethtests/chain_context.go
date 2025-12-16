// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
)

var _ core.ChainContext = (*chainContext)(nil)

type chainContext struct {
	engine consensus.Engine
	*ReaderAdapter
}

func (c *chainContext) Engine() consensus.Engine {
	return c.engine
}
