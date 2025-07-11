package sae

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook/hooktest"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

func TestExecutorClock(t *testing.T) {
	ctx := context.Background()

	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	hooks := new(hooktest.Simple)
	chainConfig := params.TestChainConfig

	const start = 42
	vm := newVM(
		ctx, t, time.Now, hooks,
		tbLogger{tb: t, level: logging.Debug + 1},
		genesisJSON(t, start, chainConfig, eoa),
	)
	t.Cleanup(func() {
		require.NoError(t, vm.Shutdown(ctx))
	})

	// SUT
	exec := vm.exec

	assertGasTimeAndExcess := func(t *testing.T, sec uint64, gasThisSec, excess gas.Gas) {
		t.Helper()
		got := exec.TimeNotThreadsafe()
		want := proxytime.New[gas.Gas](sec, 2*hooks.T) // R = 2T
		want.Tick(gasThisSec)
		if got.Cmp(want) != 0 {
			t.Errorf("%T.TimeNotThreadsafe() got %s; want %s", exec, got.String(), want.String())
		}
		if got, want := got.Excess(), excess; got != want {
			t.Errorf("%T.TimeNotThreadsafe().Excess() got %d; want %d", exec, got, want)
		}
	}
	requireGasTimeAndExcess := func(t *testing.T, sec uint64, gasThisSec, excess gas.Gas) {
		t.Helper()
		assertGasTimeAndExcess(t, sec, gasThisSec, excess)
		if t.Failed() {
			t.FailNow()
		}
	}

	hooks.T = genesisBlockGasTarget
	requireGasTimeAndExcess(t, start, 0, 0)

	builder := vm.newSimpleBlockBuilder(ctx, t)
	executeNextBlock := func(t *testing.T, timestamp uint64, txs ...*types.Transaction) *blocks.Block {
		t.Helper()
		b := builder.next(t, timestamp, txs...)
		require.NoErrorf(t, exec.EnqueueAccepted(ctx, b), "%T.EnqueueAccepted()", exec)
		require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
		return b
	}

	stub := hookstest.Stub{
		MinimumGasConsumptionFn: func(uint64) uint64 {
			// Force all transactions to use their full gas limit, which makes
			// these tests easier to reason about.
			return math.MaxUint64
		},
	}
	stub.Register(t)

	txs := simpleTxSigner{
		key:    key,
		signer: types.LatestSigner(exec.ChainConfig()),
	}

	requireGasTimeAndExcess(t, start, 0, 0) // repeated, but a reminder for readability

	hooks.T = 1_000_000 // i.e. R = 2_000_000

	// Because R = 2T, excess increases by half of the aggregate gas consumed by
	// the block.
	executeNextBlock(t, start, txs.next(100_000), txs.next(50_000), txs.next(300_000))
	requireGasTimeAndExcess(t, start, 450_000, 450_000/2)

	// If the above increase (half of gas consumed) were to be applied per
	// transaction then there would be rounding down of odd numbers of gas
	// units.
	executeNextBlock(t, start, txs.next(1e5-1), txs.next(1e5+1))
	requireGasTimeAndExcess(t, start, 650_000, 650_000/2)

	// No specific behaviour being tested; just moving to a useful time for the
	// next block.
	executeNextBlock(t, start, txs.next(1e6))
	requireGasTimeAndExcess(t, start, 1_650_000, 1_650_000/2)

	// Excess also decreases by half when the clock is fast-forwarded due to an
	// empty queue followed by a future block. Although in practice we won't
	// have blocks without transactions, they're useful to demonstrate clock
	// behaviour due to pre- and post-execution hooks.
	executeNextBlock(t, start+1)
	requireGasTimeAndExcess(t, start+1, 0, (1_650_000/2)-(350_000/2))

	// No specific behaviour being tested.
	executeNextBlock(t, start+1, txs.next(1_900_000))
	requireGasTimeAndExcess(t, start+1, 1.9e6, (1_300_000+1_900_000)/2) // show your working
	requireGasTimeAndExcess(t, start+1, 1.9e6, 1_600_000)

	// Excess depletes by the fast-forwarded (gas) time; i.e. R/2=T for each
	// empty block, but is clipped at 0.
	executeNextBlock(t, start+2)
	requireGasTimeAndExcess(t, start+2, 0, 1_550_000)
	executeNextBlock(t, start+3)
	requireGasTimeAndExcess(t, start+3, 0, 550_000)
	executeNextBlock(t, start+4)
	requireGasTimeAndExcess(t, start+4, 0, 0)
	executeNextBlock(t, start+5)
	requireGasTimeAndExcess(t, start+5, 0, 0)

	// Fast-forwarding occurs before the block is executed. If it didn't, then
	// the increase in excess would be reduced back to zero.
	executeNextBlock(t, start+6, txs.next(100_000), txs.next(50_000))
	requireGasTimeAndExcess(t, start+6, 150_000, 75_000)

	// When the gas target scales then so too must the clock's fractional
	// seconds and the gas excess.
	require.Equal(t, gas.Gas(1e6), exec.TimeNotThreadsafe().Target()) // just a reminder
	hooks.T = 2e6
	executeNextBlock(t, start+6)
	requireGasTimeAndExcess(t, start+6, 300_000, 150_000)
	hooks.T = 500_000
	executeNextBlock(t, start+6)
	requireGasTimeAndExcess(t, start+6, 75_000, 37_500)

	// The modified gas target only affects excess scaling before execution, but
	// increases/decreases are still at a rate of half the consumed/skipped gas,
	// respectively.
	executeNextBlock(t, start+6, txs.next(900_000))
	requireGasTimeAndExcess(t, start+6, 975_000, 37_500+(900_000/2))
	executeNextBlock(t, start+7)
	requireGasTimeAndExcess(t, start+7, 0, 37_500+(900_000/2)-(25_000/2))

	// Large transactions move the clock ahead of the block time.
	halfSecondOfGas := hooks.FractionSecondsOfGas(t, 1, 2)
	twoAndAHalfSeconds := 5 * halfSecondOfGas
	executeNextBlock(t, start+10, txs.next(uint64(twoAndAHalfSeconds)))
	requireFutureTime := func(t *testing.T) {
		t.Helper()
		requireGasTimeAndExcess(t, start+10+2, halfSecondOfGas, twoAndAHalfSeconds/2)
	}
	requireFutureTime(t)
	// And blocks not as far in the future then don't change the clock.
	executeNextBlock(t, start+10+1)
	requireFutureTime(t)

	t.Run("gas_price_charged", func(t *testing.T) {
		// Push the excess high enough that we have a gas price > 1.
		executeNextBlock(t, start+20)
		requireGasTimeAndExcess(t, start+20, 0, 0)
		hooks.T = 100
		executeNextBlock(t, start+20, txs.next(100_000))
		requireGasTimeAndExcess(t, start+520, 0, 50_000) // excess is arbitrary but high

		gasPrice := exec.TimeNotThreadsafe().Price()
		require.Equal(t, gas.CalculatePrice(1, 50_000, 87*hooks.T), gasPrice)
		require.True(t, gasPrice > 1, "Gas price > 1") // require.Greater allows comparison of different types #Python

		rootBefore := executeNextBlock(t, start+20).PostExecutionStateRoot()
		sdb, err := state.New(rootBefore, exec.StateCache(), nil)
		require.NoError(t, err, "state.New()")
		balBefore := sdb.GetBalance(eoa)

		txs.gasTipCap = 0
		tx0 := txs.next(170_000)
		txs.gasTipCap = 7
		tx1 := txs.next(190_000)
		txs.gasTipCap = 0
		wantCharge := uint64(170_000*gasPrice + 190_000*(gasPrice+7)) // 17 and 19 are deliberately prime

		rootAfter := executeNextBlock(t, start+20, tx0, tx1).PostExecutionStateRoot()
		sdb, err = state.New(rootAfter, exec.StateCache(), nil)
		require.NoError(t, err, "state.New()")
		got := new(uint256.Int).Sub(balBefore, sdb.GetBalance(eoa))

		if want := uint256.NewInt(wantCharge); !got.Eq(want) {
			t.Errorf("Balance reduction from gas-cost alone; got %s; want %s", got, want)
		}
	})
}
