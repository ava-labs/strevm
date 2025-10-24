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
func NewExecutor[T Result](
	tb testing.TB,
	logger logging.Logger,
	db ethdb.Database,
	config *params.ChainConfig,
	alloc types.GenesisAlloc,
	precompileAddr common.Address,
	handler parallel.Handler[T],
	workers int,
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

	proc := &processor[T]{
		par: parallel.New(handler, workers),
	}
	stub := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompileAddr: vm.NewStatefulPrecompile(proc.Run),
		},
	}
	stub.Register(tb)

	exec, err := saexec.New(gen, config, db, nil, proc, logger)
	require.NoError(tb, err)
	tb.Cleanup(exec.Close)

	return exec
}

type processor[T Result] struct {
	par *parallel.Processor[T]
}

var (
	_ hook.Points                    = (*processor[Result])(nil)
	_ vm.PrecompiledStatefulContract = new(processor[Result]).Run
)

func (*processor[T]) GasTarget(*types.Block) gas.Gas {
	return 100e6
}

func (p *processor[T]) BeforeBlock(b *types.Block, rules params.Rules, sdb *state.StateDB) error {
	return p.par.StartBlock(b, rules, sdb)
}

func (p *processor[T]) AfterBlock(b *types.Block) {
	p.par.FinishBlock(b)
}

func (p *processor[T]) Run(env vm.PrecompileEnvironment, input []byte) (ret []byte, err error) {
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
