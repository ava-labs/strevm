// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blockstest_test

import (
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/ava-labs/strevm/blocks/blockstest"
)

func TestOverrideBaseFee(t *testing.T) {
	log := saetest.NewTBLogger(t, logging.Warn)
	ctx := log.CancelOnError(t.Context())

	db := rawdb.NewMemoryDatabase()
	config := saetest.ChainConfig()

	genesis := NewGenesis(t, db, config, types.GenesisAlloc{})
	chain := NewChainBuilder(genesis, WithBlockOptions(WithLogger(log)))

	exec, err := saexec.New(genesis, chain.GetBlock, config, db, nil, &hookstest.Stub{Target: 1e6}, log)
	require.NoError(t, err, "saexec.New()")

	for baseFee := range uint64(100) {
		b := chain.NewBlock(t, nil)

		want := uint256.NewInt(baseFee + 1)
		if baseFee > 0 {
			OverrideBaseFee(t, b, want)
		}

		require.NoError(t, exec.Enqueue(ctx, b))
		require.NoError(t, b.WaitUntilExecuted(ctx))

		if baseFee == 0 {
			continue
		}
		assert.Equal(t, want.ToBig(), b.BaseFee())
	}
}
