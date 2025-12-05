// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package gastime

import (
	"testing"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/stretchr/testify/require"
)

func FuzzWorstCasePrice(f *testing.F) {
	f.Fuzz(func(
		t *testing.T,
		initTimestamp, initTarget, initExcess,
		time0, timeFrac0, used0, limit0, target0,
		time1, timeFrac1, used1, limit1, target1,
		time2, timeFrac2, used2, limit2, target2,
		time3, timeFrac3, used3, limit3, target3 uint64,
	) {
		initTarget = max(initTarget, 1)

		worstcase := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))
		actual := New(initTimestamp, gas.Gas(initTarget), gas.Gas(initExcess))
		require.LessOrEqual(t, actual.Price(), worstcase.Price())

		blocks := []struct {
			time     uint64
			timeFrac gas.Gas
			used     gas.Gas
			limit    gas.Gas
			target   gas.Gas
		}{
			{
				time:     time0,
				timeFrac: gas.Gas(timeFrac0),
				used:     gas.Gas(used0),
				limit:    gas.Gas(limit0),
				target:   gas.Gas(target0),
			},
			{
				time:     time1,
				timeFrac: gas.Gas(timeFrac1),
				used:     gas.Gas(used1),
				limit:    gas.Gas(limit1),
				target:   gas.Gas(target1),
			},
			{
				time:     time2,
				timeFrac: gas.Gas(timeFrac2),
				used:     gas.Gas(used2),
				limit:    gas.Gas(limit2),
				target:   gas.Gas(target2),
			},
			{
				time:     time3,
				timeFrac: gas.Gas(timeFrac3),
				used:     gas.Gas(used3),
				limit:    gas.Gas(limit3),
				target:   gas.Gas(target3),
			},
		}
		for _, block := range blocks {
			block.limit = max(block.used, block.limit)
			block.target = clampTarget(max(block.target, 1))
			block.timeFrac %= rateOf(block.target)

			header := &types.Header{
				Time: block.time,
			}
			hook := &hookstest.Stub{
				Target:        block.target,
				SubSecondTime: block.timeFrac,
			}

			BeforeBlock(worstcase, hook, header)
			BeforeBlock(actual, hook, header)

			require.LessOrEqual(t, actual.Price(), worstcase.Price())

			AfterBlock(worstcase, block.limit, hook, header)
			AfterBlock(actual, block.used, hook, header)
		}
	})
}
