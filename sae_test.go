package sae

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/big"
	"runtime"
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

var txsInBasicE2E = flag.Uint64("basic_e2e_tx_count", 100, "Number of transactions to use in TestBasicE2E")

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
	chain := New()
	require.NoErrorf(t, chain.Initialize(
		ctx, snowCtx,
		nil, genesisJSON, nil, nil, nil, nil, nil,
	), "%T.Initialize()", chain)

	allTxs := make([]*types.Transaction, *txsInBasicE2E)
	allTxsInMempool := make(chan struct{})
	go func() {
		defer close(allTxsInMempool)
		for nonce := range *txsInBasicE2E {
			allTxs[nonce] = types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
				Nonce:     nonce,
				To:        &wethAddr,
				Value:     big.NewInt(1),
				Gas:       params.TxGas + params.SstoreSetGas + 50_000, // arbitrary buffer
				GasTipCap: big.NewInt(0),
				GasFeeCap: new(big.Int).SetUint64(math.MaxUint64),
			})
			chain.builder.mempool <- allTxs[nonce]
		}
	}()

	requiredChunks := ((*txsInBasicE2E)*params.TxGas-1)/uint64(maxGasPerChunk) + 1
	// TODO(arr4n) change this to wait until the first empty chunk, indicating
	// that the queue has been processed.
	lastChunkTime := 200 + uint64(requiredChunks)

	t.Logf(
		"Expecting %s chunks for %s transactions @ %s gas/chunk",
		human(requiredChunks),
		human(*txsInBasicE2E),
		human(maxGasPerChunk),
	)
	start := time.Now()
	for range lastChunkTime {
		b, err := chain.BuildBlock(ctx)
		require.NoErrorf(t, err, "%T.BuildBlock()", chain)
		require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
		require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)
	}
	acceptance := time.Since(start)
	t.Logf("Accepted %s blocks in %v", human(lastChunkTime), acceptance)

	var (
		gotReceipts      []*types.Receipt
		totalGasConsumed gas.Gas
		finalStateRoot   common.Hash
		lastFilledBy     time.Time
	)
	chain.exec.chunks.Wait(ctx,
		func(cs map[uint64]*chunk) bool {
			_, ok := cs[lastChunkTime]
			return ok
		},
		func(cs map[uint64]*chunk) error {
			for timestamp := range lastChunkTime + 1 {
				got, ok := cs[timestamp]
				if !ok {
					t.Errorf("chunk at time %d not found", timestamp)
					continue
				}
				gotReceipts = append(gotReceipts, got.receipts...)
				totalGasConsumed += got.consumed

				finalStateRoot = got.stateRootPost
				if len(got.receipts) > 0 {
					lastFilledBy = got.filledBy
				}
			}
			return nil
		},
	)
	if t.Failed() {
		return
	}
	require.NoError(t, chain.Shutdown(ctx))

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

			for i, r := range gotReceipts {
				// Use `require` and `t.Fatalf()` because all receipts should be
				// the same; if one fails then they'll all probably fail in the
				// same way and there's no point being inundated with thousands
				// of identical errors.
				require.Truef(t, r.Status == 1, "tx[%d] execution succeeded", i)

				want := []*types.Log{{
					Address: wethAddr,
					Topics: []common.Hash{
						crypto.Keccak256Hash([]byte("Deposit(address,uint256)")),
						eoaAsHash,
					},
					Data:   uint256.NewInt(1).PaddedBytes(32),
					TxHash: r.TxHash,
				}}
				// Certain fields will have different meanings (TBD) in SAE so
				// we ignore them for now.
				ignore := cmpopts.IgnoreFields(types.Log{}, "BlockNumber", "BlockHash", "Index", "TxIndex")
				if diff := cmp.Diff(want, r.Logs, ignore); diff != "" {
					t.Fatalf("receipt[%d] logs diff (-want +got):\n%s", i, diff)
				}
			}
		})
	})

	t.Run("state", func(t *testing.T) {
		db := chain.exec.executeScratchSpace.db
		statedb, err := state.New(finalStateRoot, state.NewDatabase(db), nil)
		require.NoError(t, err, "state.New() at last chunk's state root")

		nTxs := uint64(len(allTxs))
		assert.Equal(t, nTxs, statedb.GetNonce(eoa), "Nonce of EOA sending txs")
		assert.Equal(t, uint256.NewInt(nTxs), statedb.GetBalance(wethAddr), "Balance of WETH contract")
	})

	// Invariants associated with a zero-length builder queue are asserted by
	// the builder itself and would result in [Chain.BuildBlock] returning an
	// error.
	chain.builder.accepted.Use(ctx, func(q *txTranche) error {
		assert.Zero(t, q.pending.Len(), "block-builder length of pending-tx queue")
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
