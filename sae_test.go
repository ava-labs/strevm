package sae

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/big"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestMain(m *testing.M) {
	flag.Parse()
	goleak.VerifyTestMain(
		m,
		goleak.IgnoreCurrent(),
		// leaky, leaky, very sneaky!
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/core/state/snapshot.(*diskLayer).generate"),
	)
}

var (
	txsInBasicE2E  = flag.Uint64("basic_e2e_tx_count", 100, "Number of transactions to use in TestBasicE2E")
	cpuProfileDest = flag.String("cpu_profile_out", "", "If non-empty, file to which pprof CPU profile is written")
)

func TestBasicE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t, level: logging.Debug + 1}
	vm := New()
	snowCompatVM := adaptor.Convert(vm)

	require.NoErrorf(t, vm.Initialize(
		ctx, snowCtx,
		nil, genesisJSON(t, chainConfig, eoa), nil, nil, nil, nil, nil,
	), "%T.Initialize()", vm)

	// TODO(arr4n) run multiple goroutines as signing txs is the actual
	// bottleneck; i.e. if we sign asynchronously then the gas/s metric
	// decreases!
	allTxs := make([]*types.Transaction, *txsInBasicE2E)
	for nonce := range *txsInBasicE2E {
		allTxs[nonce] = types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			Nonce:     nonce,
			To:        &wethAddr,
			Value:     big.NewInt(1),
			Gas:       params.TxGas + params.SstoreSetGas + 50_000, // arbitrary buffer
			GasTipCap: big.NewInt(0),
			GasFeeCap: new(big.Int).SetUint64(math.MaxUint64),
		})
		vm.newTxs <- allTxs[nonce]
	}

	if d := *cpuProfileDest; d != "" {
		t.Logf("Writing CPU profile for block acceptance and execution to %q", d)
		f, err := os.Create(d)
		require.NoErrorf(t, err, "os.Create(%q) for CPU profiling", d)
		require.NoError(t, pprof.StartCPUProfile(f), "pprof.StartCPUProfile()")
		// We don't `defer pprof.StopCPUProfile()` because we want to stop it at
		// a very specific point.
	}

	quitBlockBuilding := make(chan struct{})
	blockBuildingDone := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(blockBuildingDone)
		var numBlocks int
	BlockLoop:
		for ; ; numBlocks++ {
			select {
			case <-quitBlockBuilding:
				break BlockLoop
			default:
			}

			proposed, err := snowCompatVM.BuildBlock(ctx)
			if errors.Is(err, errWaitingForExecution) {
				numBlocks--
				continue
			}
			require.NoErrorf(t, err, "%T.BuildBlock()", snowCompatVM)

			b, err := snowCompatVM.ParseBlock(ctx, proposed.Bytes())
			require.NoErrorf(t, err, "%T.ParseBlock()", snowCompatVM)
			require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
			require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)

			bb, ok := snowCompatVM.AsRawBlock(b)
			require.Truef(t, ok, "Extracting %T from snowman.Block", bb)
		}
		acceptance := time.Since(start)
		t.Logf("Built and accepted %s blocks in %v", human(numBlocks), acceptance)
	}()

	lastTxHash := allTxs[len(allTxs)-1].Hash()
	vm.exec.receipts.Wait(ctx,
		func(r map[common.Hash]*types.Receipt) bool {
			_, ok := r[lastTxHash]
			return ok
		},
		func(map[common.Hash]*types.Receipt) error {
			close(quitBlockBuilding)
			return nil
		},
	)
	<-blockBuildingDone
	pprof.StopCPUProfile() // docs state "stops the current CPU profile, if any" so ok if we didn't start it
	if t.Failed() {
		return
	}

	lastID, err := vm.LastAccepted(ctx)
	require.NoErrorf(t, err, "%T.LastAccepted()", vm)
	lastBlock, err := vm.GetBlock(ctx, lastID)
	require.NoErrorf(t, err, "%T.GetBlock(LastAccepted())", vm)
	require.Eventuallyf(t, lastBlock.executed.Load, time.Second, 10*time.Millisecond, "executed.Load() on last %T", lastBlock)

	lastExecutedBy := lastBlock.execution.byTime
	finalStateRoot := lastBlock.execution.stateRootPost

	var (
		gotReceipts      []*types.Receipt
		totalGasConsumed gas.Gas
	)
	for ; lastBlock != nil; lastBlock = lastBlock.parent {
		gotReceipts = append(lastBlock.execution.receipts, gotReceipts...)
		for _, r := range lastBlock.execution.receipts {
			totalGasConsumed += gas.Gas(r.GasUsed)
		}
	}
	require.NoErrorf(t, vm.Shutdown(ctx), "%T.Shutdown()", vm)

	t.Run("tx_receipts", func(t *testing.T) {
		require.Equalf(t, len(allTxs), len(gotReceipts), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
		t.Logf("Executed %s txs (%s gas) in %v", human(len(gotReceipts)), human(totalGasConsumed), lastExecutedBy.Sub(start))

		var wantReceipts []*types.Receipt
		for _, tx := range allTxs {
			wantReceipts = append(wantReceipts, &types.Receipt{TxHash: tx.Hash()})
		}
		if diff := cmp.Diff(wantReceipts, gotReceipts, cmpReceiptsByHash()); diff != "" {
			t.Errorf("Execution results diff (-want +got): \n%s", diff)
		}

		t.Run("logs", func(t *testing.T) {
			var eoaAsHash common.Hash
			copy(eoaAsHash[12:], eoa[:])
			// Certain fields will have different meanings (TBD) in SAE so
			// we ignore them for now.
			ignore := cmpopts.IgnoreFields(types.Log{}, "BlockNumber", "BlockHash", "Index", "TxIndex")
			want := []*types.Log{{
				Address: wethAddr,
				Topics: []common.Hash{
					crypto.Keccak256Hash([]byte("Deposit(address,uint256)")),
					eoaAsHash,
				},
				Data: uint256.NewInt(1).PaddedBytes(32),
				// TxHash to be completed for each receipt
			}}

			for i, r := range gotReceipts {
				// Use `require` and `t.Fatalf()` because all receipts should be
				// the same; if one fails then they'll all probably fail in the
				// same way and there's no point being inundated with thousands
				// of identical errors.
				require.Truef(t, r.Status == 1, "tx[%d] execution succeeded", i)
				want[0].TxHash = r.TxHash
				if diff := cmp.Diff(want, r.Logs, ignore); diff != "" {
					t.Fatalf("receipt[%d] logs diff (-want +got):\n%s", i, diff)
				}
			}
		})
	})

	t.Run("state", func(t *testing.T) {
		db := vm.db
		statedb, err := state.New(finalStateRoot, state.NewDatabase(db), nil)
		require.NoError(t, err, "state.New() at last block's state root")

		nTxs := uint64(len(allTxs))
		assert.Equal(t, nTxs, statedb.GetNonce(eoa), "Nonce of EOA sending txs")
		assert.Equal(t, uint256.NewInt(nTxs), statedb.GetBalance(wethAddr), "Balance of WETH contract")
	})
}

func newTestPrivateKey(tb testing.TB, seed []byte) *ecdsa.PrivateKey {
	tb.Helper()
	s := crypto.NewKeccakState()
	s.Write(seed)
	key, err := ecdsa.GenerateKey(crypto.S256(), s)
	require.NoErrorf(tb, err, "ecdsa.GenerateKey(%T, %T)", crypto.S256(), s)
	return key
}

func cmpReceiptsByHash() cmp.Options {
	return cmp.Options{
		cmp.Transformer("receiptHash", func(r *types.Receipt) common.Hash {
			if r == nil {
				return common.Hash{}
			}
			return r.TxHash
		}),
	}
}

type tbLogger struct {
	logging.NoLog
	level logging.Level
	tb    testing.TB
}

var _ logging.Logger = tbLogger{}

func (l tbLogger) Info(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Info, l.tb.Logf, msg, fields...)
}

func (l tbLogger) Debug(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Debug, l.tb.Logf, msg, fields...)
}

func (l tbLogger) Error(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Error, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) Fatal(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Fatal, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) handle(when time.Time, level logging.Level, dest func(string, ...any), msg string, fields ...zap.Field) {
	if level < l.level {
		return
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}

	var keys []string
	for k := range enc.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, enc.Fields[k]))
	}

	_, file, line, _ := runtime.Caller(2)
	dest("[%s] %v %q %v - %s:%d", level, when.UnixNano(), msg, parts, file, line)
}

func TestBoundedSubtract(t *testing.T) {
	const max = math.MaxUint64
	tests := []struct {
		a, b, floor, want uint64
	}{
		{1, 2, 0, 0},
		{2, 1, 0, 1},
		{2, 1, 1, 1},
		{2, 2, 1, 1},
		{3, 1, 1, 2},
		{max, 10, max - 9, max - 9},
		{max, 10, max - 11, max - 10},
	}

	for _, tt := range tests {
		assert.Equalf(t, tt.want, boundedSubtract(tt.a, tt.b, tt.floor), "max(%d-%d, %d)", tt.a, tt.b, tt.floor)
	}
}
