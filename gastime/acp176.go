// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package gastime measures time based on the consumption of gas.
package gastime

import (
	"fmt"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/hook"
)

// BeforeBlock is intended to be called before processing a block, with the
// timestamp sourced from [hook.Points] and [types.Header].
func BeforeBlock(clock *Time, pts hook.Points, h *types.Header) {
	clock.FastForwardTo(
		h.Time,
		pts.SubSecondBlockTime(clock.Rate(), h),
	)
}

// AfterBlock is intended to be called after processing a block, with the target
// sourced from [hook.Points] and [types.Header].
func AfterBlock(clock *Time, used gas.Gas, pts hook.Points, h *types.Header) error {
	clock.Tick(used)
	target := pts.GasTargetAfter(h)
	if err := clock.SetTarget(target); err != nil {
		return fmt.Errorf("%T.SetTarget() after block: %w", clock, err)
	}
	return nil
}
