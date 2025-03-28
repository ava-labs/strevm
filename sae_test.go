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
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	humanize "github.com/dustin/go-humanize"
	"github.com/google/go-cmp/cmp"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/constraints"
)

func TestMain(m *testing.M) {
	flag.Parse()
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

var txsInBasicE2E = flag.Uint64("basic_e2e_tx_count", 100, "Number of transactions to use in TestBasicE2E")

func TestBasicE2E(t *testing.T) {
	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)

	genesis, err := json.Marshal(&core.Genesis{
		Config:     chainConfig,
		Timestamp:  0,
		Difficulty: big.NewInt(0),
		Alloc: types.GenesisAlloc{
			eoa: {
				Balance: new(uint256.Int).Not(uint256.NewInt(0)).ToBig(),
			},
		},
	})
	require.NoErrorf(t, err, "json.Marshal(%T)", &core.Genesis{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t, level: logging.Debug + 1}
	chain := New()
	require.NoErrorf(t, chain.Initialize(
		ctx, snowCtx,
		nil, genesis, nil, nil, nil, nil, nil,
	), "%T.Initialize()", chain)

	allTxs := make([]*types.Transaction, *txsInBasicE2E)
	allTxsInMempool := make(chan struct{})
	go func() {
		defer close(allTxsInMempool)
		for nonce := range *txsInBasicE2E {
			allTxs[nonce] = types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
				Nonce:     nonce,
				To:        &eoa,
				Gas:       params.TxGas,
				GasTipCap: big.NewInt(0),
				GasFeeCap: new(big.Int).SetUint64(math.MaxUint64),
			})
			chain.mempool <- allTxs[nonce]
		}
	}()

	requiredChunks := (int(*txsInBasicE2E)*int(params.TxGas)-1)/maxGasPerChunk + 1
	lastChunkTime := 100 + uint64(requiredChunks)

	t.Logf(
		"Expecting %s chunks for %s transactions @ %s gas/chunk",
		comma(requiredChunks),
		comma(*txsInBasicE2E),
		comma(maxGasPerChunk),
	)
	start := time.Now()
	for range lastChunkTime {
		b, err := chain.BuildBlock(ctx)
		require.NoErrorf(t, err, "%T.BuildBlock()", chain)
		require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
		require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)
	}
	acceptance := time.Since(start)
	t.Logf("Accepted %s blocks in %v", comma(lastChunkTime), acceptance)

	var (
		gotReceipts      []*types.Receipt
		totalGasConsumed gas.Gas
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

				if len(got.receipts) > 0 {
					lastFilledBy = got.filledBy
				}
			}
			return nil
		},
	)

	require.Equalf(t, len(allTxs), len(gotReceipts), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
	t.Logf("Executed %s txs (%s gas) in %v", comma(len(gotReceipts)), comma(totalGasConsumed), lastFilledBy.Sub(start))

	var wantReceipts []*types.Receipt
	for _, tx := range allTxs {
		wantReceipts = append(wantReceipts, &types.Receipt{TxHash: tx.Hash()})
	}
	if diff := cmp.Diff(wantReceipts, gotReceipts, chunkCmpOpts()); diff != "" {
		t.Errorf("Execution results diff (-want +got): \n%s", diff)
	}

	require.NoError(t, chain.Shutdown(ctx))

	gotNonce := chain.exec.executeScratchSpace.chunk.statedb.GetNonce(eoa)
	require.Equal(t, uint64(len(allTxs)), gotNonce, "Nonce of EOA sending txs")
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

func (l tbLogger) Error(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Error, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) Fatal(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Fatal, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) Debug(msg string, fields ...zap.Field) {
	l.handle(time.Now(), logging.Debug, l.tb.Logf, msg, fields...)
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

func comma[T constraints.Integer](x T) string {
	return humanize.Comma(int64(x))
}
