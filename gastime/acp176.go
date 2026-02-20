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
// target and gas configuration sourced from [hook.Points] and [types.Header].
func (tm *Time) AfterBlock(used gas.Gas, hooks hook.Points, h *types.Header) error {
	tm.Tick(used)
	target, cfg := hooks.GasConfigAfter(h)
	// Although [Time.SetTarget] scales the excess by the same factor as the
	// change in target, it rounds when necessary, which might alter the price
	// by a negligible amount. We therefore take a price snapshot beforehand
	// otherwise we'd call [Time.findExcessForPrice] with a different value,
	// which makes it extremely hard to test.
	p := tm.Price()
	if err := tm.SetTarget(target); err != nil {
		return fmt.Errorf("%T.SetTarget() after block: %w", tm, err)
	}
	if err := tm.setConfig(cfg, p); err != nil {
		return fmt.Errorf("%T.SetConfig() after block: %w", tm, err)
	}
	return nil
}
