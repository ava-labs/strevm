// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
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
		SubSecond(hooks, h, tm.Rate()),
	)
}

// AfterBlock is intended to be called after processing a block, with the
// gas configuration sourced from [hook.Points] and [types.Header].
func (tm *Time) AfterBlock(used gas.Gas, hooks hook.Points, h *types.Header) error {
	tm.Tick(used)
	cfg := hooks.GasConfigAfter(h)
	// TargetToExcessScaling and MinPrice are optional, so we apply them as options.
	// Apply options first to avoid setting target if config is invalid.
	var opts []Option
	if cfg.TargetToExcessScaling != nil {
		opts = append(opts, WithTargetToExcessScaling(*cfg.TargetToExcessScaling))
	}
	if cfg.MinPrice != nil {
		opts = append(opts, WithMinPrice(*cfg.MinPrice))
	}
	if len(opts) > 0 {
		if err := tm.SetOpts(opts...); err != nil {
			return err
		}
	}

	if err := tm.SetTarget(cfg.Target); err != nil {
		return fmt.Errorf("%T.SetTarget() after block: %w", tm, err)
	}
	return nil
}
