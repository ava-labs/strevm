package sae

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
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
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/eth/filters.(*FilterAPI).timeoutLoop"),
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/eth/filters.(*Subscription).Unsubscribe.func1"),
	)
}

// vmViews are different views of the same [VM] instance, each useful in
// different testing scenarios.
type vmViews struct {
	*VM                    // general
	snow block.ChainVM     // consensus integration testing
	rpc  *ethclient.Client // API testing
}

func newVM(ctx context.Context, tb testing.TB, now func() time.Time, hooks hook.Points, logger logging.Logger, genesis []byte) vmViews {
	tb.Helper()

	harness := &SinceGenesis{
		Now:   now,
		Hooks: hooks,
	}
	snow := adaptor.Convert(harness)

	snowCtx := snowtest.Context(tb, ids.Empty)
	snowCtx.Log = logger
	require.NoErrorf(tb, snow.Initialize(
		ctx, snowCtx,
		nil, genesis, nil, nil, nil, nil, nil,
	), "%T.Initialize()", snow)

	handlers, err := snow.CreateHandlers(ctx)
	require.NoErrorf(tb, err, "%T.CreateHandlers()", snow)
	server := httptest.NewServer(handlers[WSHandlerKey])
	tb.Cleanup(server.Close)

	rpcURL, err := url.Parse(server.URL)
	require.NoErrorf(tb, err, "url.Parse(%T.URL = %q)", server, server.URL)
	rpcURL.Scheme = "ws"
	client, err := ethclient.Dial(rpcURL.String())
	require.NoErrorf(tb, err, "ethclient.Dial(%T(%q))", server, rpcURL)
	tb.Cleanup(client.Close)

	return vmViews{
		VM:   harness.VM,
		snow: snow,
		rpc:  client,
	}
}

type txInclusion struct {
	blockHash common.Hash
	blockNum  uint64
	txIndex   uint // unsure why geth uses uint for this
}

func unwrapBlock(tb testing.TB, b snowman.Block) *blocks.Block {
	tb.Helper()
	bb, ok := b.(adaptor.Block[*blocks.Block])
	if !ok {
		tb.Fatalf("snowman.Block of concrete type %T; want %T", b, bb)
	}
	return bb.Unwrap()
}

func newTestPrivateKey(tb testing.TB, seed []byte) *ecdsa.PrivateKey {
	tb.Helper()
	s := crypto.NewKeccakState()
	s.Write(seed)
	key, err := ecdsa.GenerateKey(crypto.S256(), s)
	require.NoErrorf(tb, err, "ecdsa.GenerateKey(%T, %T)", crypto.S256(), s)
	return key
}

func txHashes[T interface {
	*types.Transaction | *types.Receipt
}](xs []T) []common.Hash {
	hashes := make([]common.Hash, len(xs))
	for i, x := range xs {
		switch x := any(x).(type) {
		case *types.Transaction:
			hashes[i] = x.Hash()
		case *types.Receipt:
			hashes[i] = x.TxHash
		}
	}
	return hashes
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

func setTrieDBCommitBlockIntervalLog2(tb testing.TB, val uint64) {
	old := trieDBCommitBlockIntervalLog2
	trieDBCommitBlockIntervalLog2 = val
	tb.Cleanup(func() {
		trieDBCommitBlockIntervalLog2 = old
	})
}

// uint64s is a [flag.Value] that parses comma-separated uint64 values. The
// pflag package doesn't play nicely with -test.* flags.
type uint64s []uint64

var _ flag.Value = (*uint64s)(nil)

func (us *uint64s) String() string {
	strs := make([]string, len(*us))
	for i, u := range *us {
		strs[i] = fmt.Sprint(u)
	}
	return strings.Join(strs, ",")
}

func (us *uint64s) Set(str string) error {
	*us = uint64s{}
	for _, s := range strings.Split(str, ",") {
		s = strings.TrimSpace(s)
		s = strings.ReplaceAll(s, "_", "")
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return err
		}
		*us = append(*us, u)
	}
	return nil
}
