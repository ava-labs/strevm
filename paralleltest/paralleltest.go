// Package paralleltest provides a test harness for [parallel] precompiles
// executing under SAE.
package paralleltest

import (
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/libevm/precompiles/parallel"
	"github.com/ava-labs/libevm/params"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

// Result is the required interface for results returned by a [parallel.Handler]
// registered with [NewExecutor].
type Result interface {
	Revert() bool
	Logs() []*types.Log
	ReturnData() []byte
}

// NewExecutor returns a new SAE block-execution queue with a precompile,
// registered, at the provided address, that sources results from the handler.
// The executor will have a single, genesis block, derived from the provded
// alloc.
func NewExecutor[Prefetch any, R Result, Aggregated any](
	tb testing.TB,
	logger logging.Logger,
	db ethdb.Database,
	config *params.ChainConfig,
	alloc types.GenesisAlloc,
	precompileAddr common.Address,
	handler parallel.Handler[Prefetch, R, Aggregated],
	prefetchers, processors int,
) *saexec.Executor {
	tb.Helper()

	gen, err := blocks.New(
		saetest.Genesis(tb, db, config, alloc),
		nil, nil, logger,
	)
	require.NoError(tb, err)

	require.NoError(tb, gen.MarkExecuted(
		db,
		gastime.New(gen.BuildTime(), 1, 0), time.Time{},
		big.NewInt(0), nil,
		gen.EthBlock().Root(),
	))
	require.NoError(tb, gen.MarkSynchronous())

	proc := &processor[Prefetch, R, Aggregated]{
		par: parallel.New(handler, prefetchers, processors),
	}
	stub := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompileAddr: vm.NewStatefulPrecompile(proc.Run),
		},
	}
	stub.Register(tb)

	exec, err := saexec.New(gen, config, db, nil, proc, logger)
	require.NoError(tb, err)
	tb.Cleanup(func() {
		exec.Close()
		proc.par.Close()
	})

	return exec
}

type processor[Data any, R Result, Aggregated any] struct {
	par *parallel.Processor[Data, R, Aggregated]
}

var (
	_ hook.Points                    = (*processor[any, Result, any])(nil)
	_ vm.PrecompiledStatefulContract = new(processor[any, Result, any]).Run
)

func (*processor[D, R, A]) GasTarget(*types.Block) gas.Gas {
	return 100e6
}

func (p *processor[D, R, A]) BeforeBlock(sdb *state.StateDB, rules params.Rules, b *types.Block) error {
	return p.par.StartBlock(sdb, rules, b)
}

func (p *processor[D, R, A]) AfterBlock(sdb *state.StateDB, b *types.Block, rs types.Receipts) {
	p.par.FinishBlock(sdb, b, rs)
}

func (p *processor[D, R, A]) Run(env vm.PrecompileEnvironment, input []byte) (ret []byte, err error) {
	sdb := env.StateDB()

	res, ok := p.par.Result(sdb.TxIndex())
	if !ok {
		return crypto.Keccak256([]byte("NoPreprocessorResult()")[:4]), vm.ErrExecutionReverted
	}
	if res.Revert() {
		return res.ReturnData(), vm.ErrExecutionReverted
	}

	for _, l := range res.Logs() {
		sdb.AddLog(l)
	}
	return res.ReturnData(), nil
}
