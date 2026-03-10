// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package paralleltest provides a test harness for [parallel] precompiles
// executing under SAE.
package paralleltest

import (
	"context"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/libevm/precompiles/parallel"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	saehookstest "github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

var _ blocks.Block // protect the import for [blocks.Block] comment rendering

// NewExecutor returns a new SAE block-execution queue with a precompile,
// registered at the provided address, that sources results from the
// [parallel.Handler]. The [saexec.Executor] will have a single, genesis block,
// derived from the provided alloc.
func NewExecutor[CommonData, Prefetch any, R parallel.PrecompileResult, Aggregated any](
	tb testing.TB,
	logger logging.Logger,
	db ethdb.Database,
	config *params.ChainConfig,
	alloc types.GenesisAlloc,
	precompileAddr common.Address,
	handler parallel.Handler[CommonData, Prefetch, R, Aggregated],
	prefetchers, processors int,
) (*saexec.Executor, *blockstest.ChainBuilder) {
	tb.Helper()

	xdb := saetest.NewExecutionResultsDB()
	gen := blockstest.NewGenesis(tb, db, xdb, config, alloc)
	chain := blockstest.NewChainBuilder(config, gen)

	par := parallel.New(prefetchers, processors)
	precompile := parallel.AddAsPrecompile(par, handler)
	stub := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompileAddr: vm.NewStatefulPrecompile(precompile),
		},
	}
	stub.Register(tb)

	hooks := saehookstest.NewStub(100e6)
	hooks.BeforeExecutingBlockFn = func(rules params.Rules, sdb *state.StateDB, b *types.Block) error {
		return par.StartBlock(sdb, rules, b)
	}
	hooks.AfterExecutingBlockFn = func(sdb *state.StateDB, b *types.Block, rs types.Receipts) {
		par.FinishBlock(sdb, b, rs)
	}

	src := blocks.Source(chain.GetBlock).AsHeaderSource()
	exec, err := saexec.New(gen, src, config, db, xdb, &triedb.Config{}, hooks, logger)
	require.NoError(tb, err, "saexec.New()")
	tb.Cleanup(func() {
		ctx := context.WithoutCancel(tb.Context())
		assert.NoErrorf(tb, chain.Last().WaitUntilExecuted(ctx), "%T.Last().WaitUntilExecuted()", chain)
		assert.NoErrorf(tb, exec.Close(), "%T.Close()", exec)
		par.Close()
	})

	return exec, chain
}
