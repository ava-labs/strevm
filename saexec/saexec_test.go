// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexec

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/cmputils"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saetest/weth"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		goleak.IgnoreCurrent(),
		// Despite the call to [snapshot.Tree.Disable] in [Executor.Close], this
		// still leaks at shutdown. This is acceptable as we only ever have one
		// [Executor], which we expect to be running for the entire life of the
		// process.
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/state/snapshot.(*diskLayer).generate"),
	)
}

func newExec(tb testing.TB, db ethdb.Database, hooks hook.Points, alloc types.GenesisAlloc) *Executor {
	tb.Helper()

	config := params.AllDevChainProtocolChanges
	ethGenesis := saetest.Genesis(tb, db, config, alloc)
	genesis := blockstest.NewBlock(tb, ethGenesis, nil, nil)
	require.NoErrorf(tb, genesis.MarkExecuted(db, gastime.New(0, 1, 0), time.Time{}, new(big.Int), nil, ethGenesis.Root()), "%T.MarkExecuted()", genesis)
	require.NoErrorf(tb, genesis.MarkSynchronous(), "%T.MarkSynchronous()", genesis)

	e, err := New(genesis, config, db, (*triedb.Config)(nil), hooks, saetest.NewTBLogger(tb, logging.Warn))
	require.NoError(tb, err, "New()")
	tb.Cleanup(e.Close)
	return e
}

func newExecWithMaxAlloc(tb testing.TB, db ethdb.Database, hooks hook.Points) (*Executor, *ecdsa.PrivateKey) {
	tb.Helper()
	key, alloc := saetest.KeyWithMaxAlloc(tb)
	return newExec(tb, db, hooks, alloc), key
}

func defaultHooks() *saetest.HookStub {
	return &saetest.HookStub{Target: 1e6}
}

func TestImmediateShutdownNonBlocking(t *testing.T) {
	newExec(t, rawdb.NewMemoryDatabase(), defaultHooks(), nil) // calls [Executor.Close] in test cleanup
}

func TestExecutionSynchronisation(t *testing.T) {
	ctx := context.Background()
	e := newExec(t, rawdb.NewMemoryDatabase(), defaultHooks(), nil)

	var chain []*blocks.Block
	parent := e.LastExecuted() // genesis
	for tm := range uint64(10) {
		ethB := blockstest.NewEthBlock(parent.EthBlock(), tm, nil)
		b := blockstest.NewBlock(t, ethB, parent, nil)
		chain = append(chain, b)
		parent = b
	}

	for _, b := range chain {
		require.NoError(t, e.Enqueue(ctx, b), "Enqueue()")
	}

	final := chain[len(chain)-1]
	require.NoErrorf(t, final.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted() on last-enqueued block", final)
	assert.Equal(t, final.NumberU64(), e.LastExecuted().NumberU64(), "Last-executed atomic pointer holds last-enqueued block")

	for _, b := range chain {
		assert.Truef(t, b.Executed(), "%T[%d].Executed()", b, b.NumberU64())
	}
}

func TestReceiptPropagation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	e, key := newExecWithMaxAlloc(t, rawdb.NewMemoryDatabase(), defaultHooks())
	signer := types.LatestSigner(e.ChainConfig())

	var (
		chain []*blocks.Block
		want  []types.Receipts
		nonce uint64
	)
	parent := e.LastExecuted()
	for range 10 {
		var (
			txs      types.Transactions
			receipts types.Receipts
		)

		for range 5 {
			tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
				Nonce:    nonce,
				Gas:      1e5,
				GasPrice: big.NewInt(1),
			})
			nonce++
			txs = append(txs, tx)
			receipts = append(receipts, &types.Receipt{TxHash: tx.Hash()})
		}
		want = append(want, receipts)

		ethB := blockstest.NewEthBlock(parent.EthBlock(), 0 /*time*/, txs)
		b := blockstest.NewBlock(t, ethB, parent, nil)
		chain = append(chain, b)
		require.NoError(t, e.Enqueue(ctx, b), "Enqueue()")
		parent = b
	}
	require.NoErrorf(t, parent.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted() on last-enqueued block", parent)

	var got []types.Receipts
	for _, b := range chain {
		got = append(got, b.Receipts())
	}
	if diff := cmp.Diff(want, got, cmputils.ReceiptsByTxHash()); diff != "" {
		t.Errorf("%T diff (-want +got):\n%s", got, diff)
	}
}

func TestSubscriptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	e, key := newExecWithMaxAlloc(t, rawdb.NewMemoryDatabase(), defaultHooks())
	signer := types.LatestSigner(e.ChainConfig())

	precompile := common.Address{'p', 'r', 'e'}
	stub := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompile: vm.NewStatefulPrecompile(func(env vm.PrecompileEnvironment, input []byte) (ret []byte, err error) {
				env.StateDB().AddLog(&types.Log{
					Address: precompile,
				})
				return nil, nil
			}),
		},
	}
	stub.Register(t)

	gotChainHeadEvents := saetest.NewEventCollector(e.SubscribeChainHeadEvent)
	gotChainEvents := saetest.NewEventCollector(e.SubscribeChainEvent)
	gotLogsEvents := saetest.NewEventCollector(e.SubscribeLogsEvent)
	var (
		wantChainHeadEvents []core.ChainHeadEvent
		wantChainEvents     []core.ChainEvent
		wantLogsEvents      [][]*types.Log
	)

	parent := e.LastExecuted()
	for nonce := uint64(0); nonce < 10; nonce++ {
		tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			To:       &precompile,
			GasPrice: big.NewInt(1),
			Gas:      1e6,
		})

		ethB := blockstest.NewEthBlock(parent.EthBlock(), 0 /*time*/, types.Transactions{tx})
		b := blockstest.NewBlock(t, ethB, parent, nil)
		require.NoError(t, e.Enqueue(ctx, b), "Enqueue()")
		parent = b

		wantChainHeadEvents = append(wantChainHeadEvents, core.ChainHeadEvent{
			Block: ethB,
		})
		logs := []*types.Log{{
			Address:     precompile,
			BlockNumber: b.NumberU64(),
			TxHash:      tx.Hash(),
			BlockHash:   b.Hash(),
		}}
		wantChainEvents = append(wantChainEvents, core.ChainEvent{
			Block: ethB,
			Hash:  b.Hash(),
			Logs:  logs,
		})
		wantLogsEvents = append(wantLogsEvents, logs)
	}

	opt := cmputils.BlocksByHash()
	t.Run("ChainHeadEvents", func(t *testing.T) {
		testEvents(t, gotChainHeadEvents, wantChainHeadEvents, opt)
	})
	t.Run("ChainEvents", func(t *testing.T) {
		testEvents(t, gotChainEvents, wantChainEvents, opt)
	})
	t.Run("LogsEvents", func(t *testing.T) {
		testEvents(t, gotLogsEvents, wantLogsEvents)
	})
}

func testEvents[T any](tb testing.TB, got *saetest.EventCollector[T], want []T, opts ...cmp.Option) {
	tb.Helper()
	// There is an invariant that stipulates [blocks.Block.MarkExecuted] MUST
	// occur before sending external events, which means that we can't rely on
	// [blocks.Block.WaitUntilExecuted] to avoid races.
	got.WaitForAtLeast(len(want))

	require.NoError(tb, got.Unsubscribe())
	if diff := cmp.Diff(want, got.All(), opts...); diff != "" {
		tb.Errorf("Collecting %T from event.Subscription; diff (-want +got):\n%s", want, diff)
	}
}

func TestExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	e, key := newExecWithMaxAlloc(t, rawdb.NewMemoryDatabase(), defaultHooks())
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	signer := types.LatestSigner(e.ChainConfig())

	var (
		txs  types.Transactions
		want types.Receipts
	)
	deploy := types.MustSignNewTx(key, signer, &types.LegacyTx{
		Nonce:    0,
		Data:     weth.CreationCode(),
		GasPrice: big.NewInt(1),
		Gas:      1e7,
	})
	contract := crypto.CreateAddress(eoa, 0)
	txs = append(txs, deploy)
	want = append(want, &types.Receipt{
		TxHash:          deploy.Hash(),
		ContractAddress: contract,
	})

	var eoaAsHash common.Hash
	copy(eoaAsHash[12:], eoa[:])

	rng := rand.New(rand.NewPCG(0, 0)) //nolint:gosec // Reproducibility is useful for tests
	var wantWethBalance uint64
	for nonce := uint64(1); nonce < 10; nonce++ {
		val := rng.Uint64N(100_000)
		tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			To:       &contract,
			Value:    new(big.Int).SetUint64(val),
			GasPrice: big.NewInt(1),
			Gas:      1e6,
		})
		wantWethBalance += val
		t.Logf("Depositing %d", val)

		txs = append(txs, tx)
		want = append(want, &types.Receipt{
			TxHash: tx.Hash(),
			Logs: []*types.Log{{
				Address: contract,
				TxHash:  tx.Hash(),
				Topics: []common.Hash{
					crypto.Keccak256Hash([]byte("Deposit(address,uint256)")),
					eoaAsHash,
				},
				Data: tx.Value().FillBytes(make([]byte, 32)),
			}},
		})
	}

	genesis := e.LastExecuted()
	ethB := blockstest.NewEthBlock(genesis.EthBlock(), 0, txs)
	b := blockstest.NewBlock(t, ethB, genesis, nil)

	var logIndex uint
	for i, r := range want {
		ui := uint(i) //nolint:gosec // Known to not overflow

		r.Status = 1
		r.TransactionIndex = ui
		r.BlockHash = b.Hash()
		r.BlockNumber = big.NewInt(1)

		for _, l := range r.Logs {
			l.TxIndex = ui
			l.BlockHash = b.Hash()
			l.BlockNumber = 1
			l.Index = logIndex
			logIndex++
		}
	}

	require.NoError(t, e.Enqueue(ctx, b), "Enqueue()")
	require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)

	opts := cmp.Options{
		cmpopts.IgnoreFields(
			types.Receipt{},
			"GasUsed", "CumulativeGasUsed",
			"Bloom",
		),
		cmputils.BigInts(),
	}
	if diff := cmp.Diff(want, b.Receipts(), opts); diff != "" {
		t.Errorf("%T.Receipts() diff (-want +got):\n%s", b, diff)
	}

	t.Run("committed_state", func(t *testing.T) {
		sdb, err := state.New(b.PostExecutionStateRoot(), e.StateCache(), nil)
		require.NoErrorf(t, err, "state.New(%T.PostExecutionStateRoot(), %T.StateCache(), nil)", b, e)

		if got, want := sdb.GetBalance(contract).ToBig(), new(big.Int).SetUint64(wantWethBalance); got.Cmp(want) != 0 {
			t.Errorf("After WETH deposits, got contract balance %v; want %v", got, want)
		}

		callData := append(
			crypto.Keccak256([]byte("balanceOf(address)"))[:4],
			eoaAsHash[:]...,
		)
		evm := vm.NewEVM(vm.BlockContext{Transfer: core.Transfer}, vm.TxContext{}, sdb, e.ChainConfig(), vm.Config{})
		got, _, err := evm.Call(vm.AccountRef(eoa), contract, callData, 1e6, uint256.NewInt(0))
		require.NoErrorf(t, err, "%T.Call([weth contract], [balanceOf(eoa)])", evm)
		if got, want := new(uint256.Int).SetBytes(got), uint256.NewInt(wantWethBalance); !got.Eq(want) {
			t.Errorf("WETH9.balanceOf([eoa]) got %v; want %v", got, want)
		}
	})
}

func TestGasAccounting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	hooks := &saetest.HookStub{}
	e, key := newExecWithMaxAlloc(t, rawdb.NewMemoryDatabase(), hooks)
	signer := types.LatestSigner(e.ChainConfig())

	const gasPerTx = gas.Gas(params.TxGas)
	at := func(blockTime, txs uint64, rate gas.Gas) *proxytime.Time[gas.Gas] {
		tm := proxytime.New[gas.Gas](blockTime, rate)
		tm.Tick(gas.Gas(txs) * gasPerTx)
		return tm
	}

	// If this fails then all of the tests need to be adjusted. This is cleaner
	// than polluting the test cases with a repetitive identifier.
	require.Equal(t, 2, gastime.TargetToRate, "gastime.TargetToRate assumption")

	// Steps are _not_ independent, so the execution time of one is the starting
	// time of the next.
	steps := []struct {
		target         gas.Gas
		blockTime      uint64
		numTxs         int
		wantExecutedBy *proxytime.Time[gas.Gas]
		// Because of the 2:1 ratio between Rate and Target, gas consumption
		// increases excess by half of the amount consumed, while
		// fast-forwarding reduces excess by half of the amount skipped.
		wantExcessAfter gas.Gas
		wantPriceAfter  gas.Price
	}{
		{
			target:          5 * gasPerTx,
			blockTime:       2,
			numTxs:          3,
			wantExecutedBy:  at(2, 3, 10*gasPerTx),
			wantExcessAfter: 3 * gasPerTx / 2,
			wantPriceAfter:  1, // Excess isn't high enough so price is effectively e^0
		},
		{
			target:          5 * gasPerTx,
			blockTime:       3, // fast-forward
			numTxs:          12,
			wantExecutedBy:  at(4, 2, 10*gasPerTx),
			wantExcessAfter: 12 * gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          5 * gasPerTx,
			blockTime:       4, // no fast-forward so starts at last execution time
			numTxs:          20,
			wantExecutedBy:  at(6, 2, 10*gasPerTx),
			wantExcessAfter: (12 + 20) * gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          5 * gasPerTx,
			blockTime:       7, // fast-forward equivalent of 8 txs
			numTxs:          16,
			wantExecutedBy:  at(8, 6, 10*gasPerTx),
			wantExcessAfter: (12 + 20 - 8 + 16) * gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          10 * gasPerTx, // double gas/block --> halve ticking rate
			blockTime:       8,             // no fast-forward
			numTxs:          4,
			wantExecutedBy:  at(8, (6*2)+4, 20*gasPerTx), // starting point scales
			wantExcessAfter: (2*(12+20-8+16) + 4) * gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          5 * gasPerTx, // back to original
			blockTime:       8,
			numTxs:          5,
			wantExecutedBy:  at(8, 6+(4/2)+5, 10*gasPerTx),
			wantExcessAfter: ((12 + 20 - 8 + 16) + 4/2 + 5) * gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          5 * gasPerTx,
			blockTime:       20, // more than double the last executed-by time, reduces excess to 0
			numTxs:          1,
			wantExecutedBy:  at(20, 1, 10*gasPerTx),
			wantExcessAfter: gasPerTx / 2,
			wantPriceAfter:  1,
		},
		{
			target:          5 * gasPerTx,
			blockTime:       21,                                 // fast-forward so excess is 0
			numTxs:          30 * gastime.TargetToExcessScaling, // deliberate, see below
			wantExecutedBy:  at(21, 30*gastime.TargetToExcessScaling, 10*gasPerTx),
			wantExcessAfter: 3 * ((5 * gasPerTx /*T*/) * gastime.TargetToExcessScaling /* == K */),
			// Excess is now 3·K so the price is e^3 = 20.09
			wantPriceAfter: 20,
		},
		{
			target:          5 * gasPerTx,
			blockTime:       22, // no fast-forward
			numTxs:          10 * gastime.TargetToExcessScaling,
			wantExecutedBy:  at(21, 40*gastime.TargetToExcessScaling, 10*gasPerTx),
			wantExcessAfter: 4 * ((5 * gasPerTx /*T*/) * gastime.TargetToExcessScaling /* == K */),
			wantPriceAfter:  gas.Price(math.Floor(math.Pow(math.E, 4 /* <----- NB */))),
		},
	}

	parent := e.LastExecuted()
	var nonce uint64
	for i, step := range steps {
		hooks.Target = step.target

		txs := make(types.Transactions, step.numTxs)
		for i := range txs {
			txs[i] = types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
				Nonce:     nonce,
				To:        &common.Address{},
				Gas:       params.TxGas,
				GasTipCap: big.NewInt(0),
				GasFeeCap: big.NewInt(100),
			})
			nonce++
		}

		ethB := blockstest.NewEthBlock(parent.EthBlock(), step.blockTime, txs)
		b := blockstest.NewBlock(t, ethB, parent, nil)
		require.NoError(t, e.Enqueue(ctx, b), "Enqueue()")
		require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
		parent = b

		for desc, got := range map[string]*gastime.Time{
			fmt.Sprintf("%T.ExecutedByGasTime()", b): b.ExecutedByGasTime(),
			fmt.Sprintf("%T.TimeNotThreadSafe()", e): e.TimeNotThreadsafe(),
		} {
			opt := proxytime.CmpOpt[gas.Gas](proxytime.IgnoreRateInvariants)
			if diff := cmp.Diff(step.wantExecutedBy, got.Time, opt); diff != "" {
				t.Errorf("%s diff (-want +got):\n%s", desc, diff)
			}
		}

		t.Run("CumulativeGasUsed", func(t *testing.T) {
			for i, r := range b.Receipts() {
				ui := uint64(i + 1) //nolint:gosec // Known to not overflow
				assert.Equalf(t, ui*params.TxGas, r.CumulativeGasUsed, "%T.Receipts()[%d]", b, i)
			}
		})

		if t.Failed() {
			// Future steps / tests may be corrupted and false-positive errors
			// aren't helpful.
			break
		}

		t.Run("gas_price", func(t *testing.T) {
			tm := e.TimeNotThreadsafe()
			assert.Equalf(t, step.wantExcessAfter, tm.Excess(), "%T.Excess()", tm)
			assert.Equalf(t, step.wantPriceAfter, tm.Price(), "%T.Price()", tm)

			wantBaseFee := gas.Price(1)
			if i > 0 {
				wantBaseFee = steps[i-1].wantPriceAfter
			}
			require.Truef(t, b.BaseFee().IsUint64(), "%T.BaseFee().IsUint64()", b)
			assert.Equalf(t, wantBaseFee, gas.Price(b.BaseFee().Uint64()), "%T.BaseFee().Uint64()", b)
		})
	}
}
