package sae

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
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
	"github.com/ava-labs/libevm/core"
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

	genesisJSON, err := json.Marshal(genesis(t, chainConfig, eoa))
	require.NoErrorf(t, err, "json.Marshal(%T)", &core.Genesis{})

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t, level: logging.Debug + 1}
	vm := New()
	snowCompatVM := adaptor.Convert(vm)

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
		vm.builder.newTxs <- &transaction{
			tx:   allTxs[nonce],
			from: eoa, // in reality this is populated by an RPC-server goroutine
		}
	}

	require.NoErrorf(t, vm.Initialize(
		ctx, snowCtx,
		nil, genesisJSON, nil, nil, nil, nil, nil,
	), "%T.Initialize()", vm)

	if d := *cpuProfileDest; d != "" {
		t.Logf("Writing CPU profile for block acceptance and execution to %q", d)
		f, err := os.Create(d)
		require.NoErrorf(t, err, "os.Create(%q) for CPU profiling", d)
		require.NoError(t, pprof.StartCPUProfile(f), "pprof.StartCPUProfile()")
		// We don't `defer pprof.StopCPUProfile()` because we want to stop it at
		// a very specific point.
	}

	var finalStateRoot common.Hash
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

			b, err := snowCompatVM.BuildBlock(ctx)
			require.NoErrorf(t, err, "%T.BuildBlock()", snowCompatVM)
			require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
			require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)

			bb, ok := snowCompatVM.AsRawBlock(b)
			require.Truef(t, ok, "Extracting %T from snowman.Block", bb)
			finalStateRoot = bb.Root()
		}
		acceptance := time.Since(start)
		t.Logf("Built and accepted %s blocks in %v", human(numBlocks), acceptance)
	}()

	lastTxHash := allTxs[len(allTxs)-1].Hash()
	vm.exec.chunks.Wait(ctx,
		func(res *executionResults) bool {
			_, ok := res.receipts[lastTxHash]
			return ok
		},
		func(res *executionResults) error {
			close(quitBlockBuilding)
			return nil
		},
	)
	<-blockBuildingDone

	var (
		gotReceipts      []*types.Receipt
		totalGasConsumed gas.Gas
		lastFilledBy     time.Time
	)
	vm.exec.chunks.UseThenSignal(ctx, func(res *executionResults) error {
		var lastChunkTime uint64
		for t := range res.chunks {
			if t != math.MaxUint64 && t > lastChunkTime {
				lastChunkTime = t
			}
		}

		for timestamp := range lastChunkTime + 1 {
			got, ok := res.chunks[timestamp]
			if !ok {
				t.Errorf("chunk at time %d not found", timestamp)
				continue
			}
			gotReceipts = append(gotReceipts, got.receipts...)
			totalGasConsumed += got.consumed

			if len(got.receipts) > 0 {
				lastFilledBy = got.filledBy
			}
		}
		return nil
	})
	pprof.StopCPUProfile() // docs state "stops the current CPU profile, if any" so ok if we didn't start it
	if t.Failed() {
		return
	}
	require.NoError(t, vm.Shutdown(ctx))

	t.Run("tx_receipts", func(t *testing.T) {
		require.Equalf(t, len(allTxs), len(gotReceipts), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
		t.Logf("Executed %s txs (%s gas) in %v", human(len(gotReceipts)), human(totalGasConsumed), lastFilledBy.Sub(start))

		var wantReceipts []*types.Receipt
		for _, tx := range allTxs {
			wantReceipts = append(wantReceipts, &types.Receipt{TxHash: tx.Hash()})
		}
		if diff := cmp.Diff(wantReceipts, gotReceipts, chunkCmpOpts()); diff != "" {
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
		db := vm.exec.executeScratchSpace.db
		statedb, err := state.New(finalStateRoot, state.NewDatabase(db), nil)
		require.NoError(t, err, "state.New() at last chunk's state root")

		nTxs := uint64(len(allTxs))
		assert.Equal(t, nTxs, statedb.GetNonce(eoa), "Nonce of EOA sending txs")
		assert.Equal(t, uint256.NewInt(nTxs), statedb.GetBalance(wethAddr), "Balance of WETH contract")
	})

	// Invariants associated with a zero-length builder queue are asserted by
	// the builder itself and would result in [VM.BuildBlock] returning an
	// error.
	vm.builder.tranches.UseThenSignal(ctx, func(trs *tranches) error {
		assert.Zero(t, trs.accepted.pending.Len(), "block-builder length of pending-tx queue")
		return nil
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

func chunkCmpOpts() cmp.Options {
	return cmp.Options{
		cmp.AllowUnexported(chunk{}),
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
