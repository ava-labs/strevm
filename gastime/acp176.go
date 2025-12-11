// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"fmt"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"

	"github.com/ava-labs/strevm/hook"
)

// BeforeBlock is intended to be called before processing a block, with the
// timestamp sourced from [hook.Points] and [types.Header].
func (tm *Time) BeforeBlock(hooks hook.Points, h *types.Header) {
	tm.FastForwardTo(
		h.Time,
		hooks.SubSecondBlockTime(tm.Rate(), h),
	)
}

// AfterBlock is intended to be called after processing a block, with the target
// sourced from [hook.Points] and [types.Header].
func (tm *Time) AfterBlock(used gas.Gas, hooks hook.Points, h *types.Header) error {
	tm.Tick(used)
	target := hooks.GasTargetAfter(h)
	if err := tm.SetTarget(target); err != nil {
		return fmt.Errorf("%T.SetTarget() after block: %w", tm, err)
	}
	return nil
}
