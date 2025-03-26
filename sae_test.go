package sae

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	humanize "github.com/dustin/go-humanize"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestMain(m *testing.M) {
	flag.Parse()
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

var txsInBlock = flag.Uint64("basic_block_tx_count", 100, "Number of transactions to use in TestBasicRoundTrip")

func TestBasicRoundTrip(t *testing.T) {
	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t, level: logging.Debug + 1}
	chain := New()
	require.NoErrorf(t, chain.Initialize(
		ctx, snowCtx,
		nil, nil, nil, nil, nil, nil, nil,
	), "%T.Initialize()", chain)

	header := &types.Header{
		Number:     big.NewInt(1),
		Time:       1,
		ParentHash: common.Hash(chain.genesis),
	}
	body := types.Body{
		Transactions: []*types.Transaction{},
	}
	for nonce := range *txsInBlock {
		tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce: nonce,
			To:    &eoa,
			Gas:   params.TxGas,
		})
		body.Transactions = append(body.Transactions, tx)
	}

	requiredChunks := (len(body.Transactions)*int(params.TxGas)-1)/maxGasPerChunk + 1
	waitForChunks := requiredChunks + 2 // extras for each of genesis and chunk-reporting blocks

	blocks := []snowman.Block{
		asSnowmanBlock(ctx, t, chain, types.NewBlockWithHeader(header).WithBody(body)),
		asSnowmanBlock(
			ctx, t, chain,
			types.NewBlockWithHeader(&types.Header{
				Number:     new(big.Int).Add(header.Number, big.NewInt(1)),
				Time:       header.Time + uint64(requiredChunks),
				ParentHash: header.Hash(),
			}),
		),
	}
	for _, b := range blocks {
		require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
	}

	start := time.Now()
	for _, b := range blocks {
		require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)
	}
	for range waitForChunks {
		<-chain.exec.chunkFilled
	}

	var (
		gotReceipts      []*types.Receipt
		totalGasConsumed gas.Gas
		lastFilledBy     time.Time
	)
	chain.exec.chunks.Use(ctx, func(cs map[uint64]*chunk) error {
		for timestamp := range uint64(waitForChunks) {
			got, ok := cs[timestamp]
			if !ok {
				t.Errorf("chunk at time %d not found", timestamp)
				continue
			}
			gotReceipts = append(gotReceipts, got.receipts...)
			totalGasConsumed += got.consumed
			lastFilledBy = got.filledBy
		}
		return nil
	})

	require.Equalf(t, len(body.Transactions), len(gotReceipts), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
	{
		gas := humanize.Comma(int64(totalGasConsumed))
		t.Logf("Executed %d txs (%s gas) in %v", len(gotReceipts), gas, lastFilledBy.Sub(start))
	}

	var wantReceipts []*types.Receipt
	for _, tx := range body.Transactions {
		wantReceipts = append(wantReceipts, &types.Receipt{TxHash: tx.Hash()})
	}
	if diff := cmp.Diff(wantReceipts, gotReceipts, chunkCmpOpts()); diff != "" {
		t.Errorf("Execution results diff (-want +got): \n%s", diff)
	}

	require.NoError(t, chain.Shutdown(ctx))
	gotNonce := chain.exec.executeScratchSpace.statedb.GetNonce(eoa)
	require.Equal(t, uint64(len(body.Transactions)), gotNonce, "Nonce of EOA sending txs")
}

func newTestPrivateKey(tb testing.TB, seed []byte) *ecdsa.PrivateKey {
	tb.Helper()
	s := crypto.NewKeccakState()
	s.Write(seed)
	key, err := ecdsa.GenerateKey(crypto.S256(), s)
	require.NoErrorf(tb, err, "ecdsa.GenerateKey(%T, %T)", crypto.S256(), s)
	return key
}

func asSnowmanBlock(ctx context.Context, tb testing.TB, chain *Chain, b *types.Block) snowman.Block {
	tb.Helper()
	buf, err := rlp.EncodeToBytes(b)
	require.NoErrorf(tb, err, "rlp.EncodeToBytes(%T)", &types.Block{})

	block, err := chain.ParseBlock(ctx, buf)
	require.NoErrorf(tb, err, "%T.ParseBlock()", chain)
	return block
}

func chunkCmpOpts() cmp.Options {
	return cmp.Options{
		cmp.AllowUnexported(chunk{}),
		cmpopts.IgnoreFields(chunk{}, "stateRoot"),
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
	l.handle(logging.Error, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) Fatal(msg string, fields ...zap.Field) {
	l.handle(logging.Fatal, l.tb.Errorf, msg, fields...)
}

func (l tbLogger) Debug(msg string, fields ...zap.Field) {
	l.handle(logging.Debug, l.tb.Logf, msg, fields...)
}

func (l tbLogger) handle(level logging.Level, dest func(string, ...any), msg string, fields ...zap.Field) {
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
	dest("[%s] %q %v - %s:%d", level, msg, parts, file, line)
}
