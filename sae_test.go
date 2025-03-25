package sae

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"runtime"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
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

	hdr := &types.Header{
		Number: big.NewInt(42),
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

	blockBuf, err := rlp.EncodeToBytes(types.NewBlockWithHeader(hdr).WithBody(body))
	require.NoErrorf(t, err, "rlp.EncodeToBytes(%T)", &types.Block{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t}
	chain := New()
	require.NoErrorf(t, chain.Initialize(
		ctx, snowCtx,
		nil, nil, nil, nil, nil, nil, nil,
	), "%T.Initialize()", chain)

	block, err := chain.ParseBlock(ctx, blockBuf)
	require.NoErrorf(t, err, "%T.ParseBlock()", chain)
	require.NoErrorf(t, block.Verify(ctx), "%T.Verify()", block)

	start := time.Now()
	require.NoErrorf(t, block.Accept(ctx), "%T.Accept()", block)
	got := <-chain.execResults
	end := time.Now()
	{
		// TODO(arr4n) using `CumulativeGasUsed` will underestimate gas used
		// once chunk filling is properly implemented.
		last := got.receipts[len(got.receipts)-1]
		gas := humanize.Comma(int64(last.CumulativeGasUsed))
		t.Logf("Executed %d txs (%s gas) in %v", len(body.Transactions), gas, end.Sub(start))
	}

	want := &chunk{
		usedGas: params.TxGas * uint64(len(body.Transactions)),
	}
	for _, tx := range body.Transactions {
		want.receipts = append(want.receipts, &types.Receipt{TxHash: tx.Hash()})
	}
	if diff := cmp.Diff(want, got, chunkCmpOpts()); diff != "" {
		t.Errorf("Execution results diff (-want +got): \n%s", diff)
	}

	require.NoError(t, chain.Shutdown(ctx))
	gotNonce := chain.exec.executeScratchSpace.statedb.GetNonce(eoa)
	require.Equal(t, uint64(len(body.Transactions)), gotNonce, "Nonce of EOA sending txs")
}

func newTestPrivateKey(t *testing.T, seed []byte) *ecdsa.PrivateKey {
	t.Helper()
	s := crypto.NewKeccakState()
	s.Write(seed)
	key, err := ecdsa.GenerateKey(crypto.S256(), s)
	require.NoErrorf(t, err, "ecdsa.GenerateKey(%T, %T)", crypto.S256(), s)
	return key
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
	tb testing.TB
}

func (l tbLogger) Error(msg string, fields ...zap.Field) {
	l.handle(logging.Error, msg, fields...)
}

func (l tbLogger) Fatal(msg string, fields ...zap.Field) {
	l.handle(logging.Fatal, msg, fields...)
}

func (l tbLogger) handle(level logging.Level, msg string, fields ...zap.Field) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		var val any = f.Interface
		switch v := val.(type) {
		case ids.ID:
			val = fmt.Sprintf("%#x", v)
		}
		parts[i] = fmt.Sprintf("%q=%v", f.Key, val)
	}
	_, file, line, _ := runtime.Caller(2)
	l.tb.Errorf("[%s] %q %v (%s:%d)", level, msg, parts, file, line)
}
