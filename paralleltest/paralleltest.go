// Package paralleltest provides a test harness for [parallel] precompiles
// executing under SAE.
package paralleltest

import (
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/libevm/precompiles/parallel"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saexec"
)

// NewExecutor returns a new SAE block-execution queue with a precompile,
// registered, at the provided address, that sources results from the handler.
// The executor will have a single, genesis block, derived from the provded
// alloc.
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

	gen := blockstest.NewGenesis(tb, db, config, alloc)
	chain := blockstest.NewChainBuilder(gen)

	par := parallel.New(prefetchers, processors)
	precompile := parallel.AddAsPrecompile(par, handler)
	stub := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompileAddr: vm.NewStatefulPrecompile(precompile),
		},
	}
	stub.Register(tb)

	exec, err := saexec.New(gen, chain.GetBlock, config, db, nil, &hooks{par}, logger)
	require.NoError(tb, err)
	tb.Cleanup(func() {
		exec.Close()
		par.Close()
	})

	return exec, chain
}

type hooks struct {
	par *parallel.Processor
}

var _ hook.Points = (*hooks)(nil)

func (*hooks) GasTarget(*types.Block) gas.Gas {
	return 100e6
}

func (*hooks) SubSecondBlockTime(*types.Block) gas.Gas {
	return 0
}

func (h *hooks) BeforeBlock(rules params.Rules, sdb *state.StateDB, b *types.Block) error {
	return h.par.StartBlock(sdb, rules, b)
}

func (h *hooks) AfterBlock(sdb *state.StateDB, b *types.Block, rs types.Receipts) {
	h.par.FinishBlock(sdb, b, rs)
}
