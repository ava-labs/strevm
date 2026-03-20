// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package types

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
)

type (
	// A BlockSource returns a Block that matches both a hash and number, and a
	// boolean indicating if such a block was found.
	BlockSource func(hash common.Hash, number uint64) (*types.Block, bool)
	// A HeaderSource is equivalent to a [BlockSource] except that it only
	// returns the block header.
	HeaderSource func(hash common.Hash, number uint64) (*types.Header, bool)
)
