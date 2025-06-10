package sae

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/big"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/ids"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/weth"
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
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/eth/filters.(*FilterAPI).timeoutLoop"),
		goleak.IgnoreTopFunction("github.com/ava-labs/libevm/eth/filters.(*Subscription).Unsubscribe.func1"),
	)
}

var (
	txsInBasicE2E  = flag.Uint64("basic_e2e_tx_count", 1_000, "Number of transactions to use in TestBasicE2E")
	cpuProfileDest = flag.String("cpu_profile_out", "", "If non-empty, file to which pprof CPU profile is written")
)

type stubHooks struct {
	T gas.Gas
}

func (h *stubHooks) GasTarget(parent *types.Block) gas.Gas {
	return h.T
}

func TestBasicE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	t.Logf("Sending all txs as EOA %v", eoa)
	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)

	now := time.Unix(0, 0)

	harnessVM := &SinceGenesis{
		Now: func() time.Time { return now },
		Hooks: &stubHooks{
			T: 2e6,
		},
	}
	snowCompatVM := adaptor.Convert(harnessVM)

	snowCtx := snowtest.Context(t, ids.Empty)
	snowCtx.Log = tbLogger{tb: t, level: logging.Debug + 1}
	require.NoErrorf(t, harnessVM.Initialize(
		ctx, snowCtx,
		nil, genesisJSON(t, chainConfig, eoa), nil, nil, nil, nil, nil,
	), "%T.Initialize()", harnessVM)

	vm := harnessVM.VM

	t.Run("convert_genesis_to_async", func(t *testing.T) {
		require.NoError(t, vm.blocks.Use(ctx, func(bm blockMap) error {
			require.Equal(t, 1, len(bm))
			return nil
		}))

		last, err := vm.LastAccepted(ctx)
		require.NoErrorf(t, err, "%T.LastAccepted()", vm)
		block, err := vm.GetBlock(ctx, last)
		require.NoErrorf(t, err, "%T.GetBlock(%[1]T.LastAccepted())", vm)

		assert.Equal(t, uint64(0), block.Height(), "block height")
		assert.Nil(t, block.parent, "parent")
		assert.Nil(t, block.lastSettled, "last settled")
		assert.True(t, block.executed.Load(), "executed")

		for k, ptr := range map[string]*atomic.Pointer[Block]{
			"accepted": &vm.last.accepted,
			"executed": &vm.last.executed,
			"settled":  &vm.last.settled,
		} {
			if diff := cmp.Diff(block, ptr.Load(), cmpBlocks()); diff != "" {
				t.Errorf("%T.last.%s diff (-want +got):\n%s", vm, k, diff)
			}
		}
	})

	handlers, err := vm.CreateHandlers(ctx)
	require.NoErrorf(t, err, "%T.CreateHandlers()", vm)
	rpcServer := httptest.NewServer(handlers[WSHandlerKey])
	t.Cleanup(rpcServer.Close)

	rpcURL, err := url.Parse(rpcServer.URL)
	require.NoErrorf(t, err, "url.Parse(%T.URL = %q)", rpcServer, rpcServer.URL)
	rpcURL.Scheme = "ws"
	rpcClient, err := ethclient.Dial(rpcURL.String())
	require.NoErrorf(t, err, "ethclient.Dial(%T(%q))", rpcServer, rpcURL)
	t.Cleanup(rpcClient.Close)

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
		require.NoError(t, rpcClient.SendTransaction(ctx, allTxs[nonce]))
	}

	numPendingTxs := func(ctx context.Context) (int, error) {
		return sink.FromPriorityMutex(
			ctx, vm.mempool, sink.MaxPriority,
			func(_ <-chan sink.Priority, mp *queue.Priority[*pendingTx]) (int, error) {
				return mp.Len(), nil
			},
		)
	}
	mempoolPopulated := func() bool {
		n, err := numPendingTxs(ctx)
		return err == nil && n == len(allTxs)
	}
	require.Eventually(t, mempoolPopulated, time.Second, 5*time.Millisecond, "mempool populated")

	if d := *cpuProfileDest; d != "" {
		t.Logf("Writing CPU profile for block acceptance and execution to %q", d)
		f, err := os.Create(d)
		require.NoErrorf(t, err, "os.Create(%q) for CPU profiling", d)
		require.NoError(t, pprof.StartCPUProfile(f), "pprof.StartCPUProfile()")
		// We don't `defer pprof.StopCPUProfile()` because we want to stop it at
		// a very specific point.
	}

	headEvents := make(chan *types.Header, len(allTxs))
	headSub, err := rpcClient.SubscribeNewHead(ctx, headEvents)
	require.NoErrorf(t, err, "%T.SubscribeNewHead()", rpcClient)

	filter := ethereum.FilterQuery{Addresses: []common.Address{wethAddr}}
	logEvents := make(chan types.Log, len(allTxs))
	logSub, err := rpcClient.SubscribeFilterLogs(ctx, filter, logEvents)
	require.NoErrorf(t, err, "%T.SubscribeFilterLogs()", err)

	var (
		acceptedBlocks      []*Block
		blockWithLastTx     *Block
		waitingForExecution int
	)
	start := time.Now()
	for numBlocks := 0; ; numBlocks++ {
		lastID, err := snowCompatVM.LastAccepted(ctx)
		require.NoErrorf(t, err, "%T.LastAccepted()", snowCompatVM)
		require.NoErrorf(t, snowCompatVM.SetPreference(ctx, lastID), "%T.SetPreference(LastAccepted())", snowCompatVM)

		proposed, err := snowCompatVM.BuildBlock(ctx)
		if errors.Is(err, errWaitingForExecution) {
			numBlocks--
			waitingForExecution++
			continue
		}
		if errors.Is(err, errNoopBlock) {
			numBlocks--
			now = now.Add(time.Second)
			continue
		}
		require.NoErrorf(t, err, "%T.BuildBlock() #%d", snowCompatVM, numBlocks)

		b, err := snowCompatVM.ParseBlock(ctx, proposed.Bytes())
		require.NoErrorf(t, err, "%T.ParseBlock()", snowCompatVM)
		require.Equal(t, now.Truncate(time.Second), b.Timestamp())

		require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
		require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)

		bb, ok := snowCompatVM.AsRawBlock(b)
		require.Truef(t, ok, "Extracting %T from snowman.Block", bb)
		acceptedBlocks = append(acceptedBlocks, bb)

		now = now.Add(900 * time.Millisecond)

		n, err := numPendingTxs(ctx)
		require.NoError(t, err, "inspect mempool for num pending txs")
		if n > 0 {
			continue
		}
		if blockWithLastTx == nil {
			blockWithLastTx = bb
			t.Logf("Last tx included in block %d", bb.NumberU64())
		} else if s := bb.lastSettled; s != nil && s.NumberU64() >= blockWithLastTx.NumberU64() {
			t.Logf("Built and accepted %s blocks in %v", human(numBlocks), time.Since(start))
			t.Logf("Consensus waited for execution %s times", human(waitingForExecution))
			break
		}
	}
	require.NoError(t, vm.exec.queueCleared.Wait(ctx))

	t.Cleanup(func() {
		// See block pruning at the end of [VM.AcceptBlock]
		n := inMemoryBlockCount.Load()
		acceptedBlocks = nil
		blockWithLastTx = nil
		runtime.GC()
		t.Logf("After GC, in-memory block count: %d => %d", n, inMemoryBlockCount.Load())
	})

	pprof.StopCPUProfile() // docs state "stops the current CPU profile, if any" so ok if we didn't start it
	if t.Failed() {
		return
	}

	ctx = context.Background() // original ctx had a timeout in case of execution deadlock, but we're past that

	lastID, err := vm.LastAccepted(ctx)
	require.NoErrorf(t, err, "%T.LastAccepted()", vm)
	lastBlock, err := vm.GetBlock(ctx, lastID)
	require.NoErrorf(t, err, "%T.GetBlock(LastAccepted())", vm)
	require.Eventuallyf(t, lastBlock.executed.Load, time.Second, 10*time.Millisecond, "executed.Load() on last %T", lastBlock)

	t.Run("persisted_block_hashes", func(t *testing.T) {
		t.Parallel()

		t.Run("with_last_tx", func(t *testing.T) {
			b := blockWithLastTx

			t.Run("last_settled_block", func(t *testing.T) {
				safe := rpc.SafeBlockNumber
				settled, err := rpcClient.HeaderByNumber(ctx, big.NewInt(safe.Int64()))
				require.NoErrorf(t, err, "%T.HeaderByNumber(%d %q)", rpcClient, safe, safe)

				assert.GreaterOrEqual(t, settled.Number.Cmp(b.Number()), 0, "last-settled height >= block including last tx")
			})
			assert.Equalf(t, b.execution.stateRootPost, lastBlock.Root(), "%T.Root() of last block must be post-execution root of block including last tx", lastBlock)

			// The following tests that [rawdb.WriteTxLookupEntriesByBlock] has
			// been called correctly.
			lastTxHash := allTxs[len(allTxs)-1].Hash()
			got, err := rpcClient.TransactionReceipt(ctx, lastTxHash)
			require.NoErrorf(t, err, "%T.TransactionReceipt(%#x [last tx])", rpcClient, lastTxHash)
			assert.Equal(t, b.Hash(), got.BlockHash, "block hash of last tx")
			assert.Equal(t, uint(len(b.Transactions())-1), got.TransactionIndex, "index of last tx")
		})

		_, got := rawdb.ReadAllCanonicalHashes(vm.db, 1, lastBlock.NumberU64()+1, math.MaxInt)
		var want []common.Hash
		for _, b := range acceptedBlocks {
			want = append(want, b.Hash())
		}
		assert.Equal(t, want, got, "rawdb.ReadAllCanonicalHashes(1...)")
	})

	t.Run("api", func(t *testing.T) {
		t.Parallel()
		t.Run("tx_receipts", func(t *testing.T) {
			t.Parallel()
			var (
				gotReceipts      []types.Receipts
				totalGasConsumed gas.Gas
			)
			for i := range lastBlock.NumberU64() {
				num := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(i))
				rs, err := rpcClient.BlockReceipts(ctx, num)
				require.NoErrorf(t, err, "%T.BlockReceipts(%d)", rpcClient, i)
				gotReceipts = append(gotReceipts, rs)

				for _, r := range rs {
					totalGasConsumed += gas.Gas(r.GasUsed)
				}
			}
			gotReceiptsConcat := slices.Concat(gotReceipts...)

			require.Equalf(t, len(allTxs), len(gotReceiptsConcat), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
			lastExecutedBy := blockWithLastTx.execution.byTime
			t.Logf("Executed %s txs (%s gas) in %v", human(len(gotReceiptsConcat)), human(totalGasConsumed), lastExecutedBy.Sub(start))

			t.Run("vs_sent_txs", func(t *testing.T) {
				t.Parallel()

				var wantReceipts types.Receipts
				for _, tx := range allTxs {
					wantReceipts = append(wantReceipts, &types.Receipt{TxHash: tx.Hash()})
				}
				if diff := cmp.Diff(wantReceipts, gotReceiptsConcat, cmpReceiptsByHash()); diff != "" {
					t.Errorf("Execution results diff (-want +got): \n%s", diff)
				}
			})

			t.Run("vs_block_contents", func(t *testing.T) {
				t.Parallel()

				for i, b := range acceptedBlocks {
					fromAPI := gotReceipts[i+1 /*skips genesis*/]
					fromDB := rawdb.ReadReceipts(vm.db, b.Hash(), b.NumberU64(), b.Time(), vm.exec.chainConfig)

					if diff := cmp.Diff(fromDB, fromAPI, cmpBigInts()); diff != "" {
						t.Errorf("Block %d receipts diff vs db: -rawdb.ReadReceipts() +%T.BlockReceipts():\n%s", b.NumberU64(), rpcClient, diff)
					}
					if diff := cmp.Diff(txHashes(b.Transactions()), txHashes(fromAPI)); diff != "" {
						t.Errorf("Block %d receipt hashes diff vs proposed block: -%T.Transactions() +%T.BlockReceipts():\n%s", b.NumberU64(), b, rpcClient, diff)
					}

					if b.Hash() == blockWithLastTx.Hash() {
						break
					}
				}
			})

			t.Run("logs", func(t *testing.T) {
				t.Parallel()

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

				logSub.Unsubscribe()
				require.NoErrorf(t, <-logSub.Err(), "receive on %T.Err()", logSub)

				for i, r := range slices.Concat(gotReceipts...) {
					// Use `require` and `t.Fatalf()` because all receipts should be
					// the same; if one fails then they'll all probably fail in the
					// same way and there's no point being inundated with thousands
					// of identical errors.
					require.Truef(t, r.Status == 1, "tx[%d] execution succeeded", i)
					want[0].TxHash = r.TxHash
					if diff := cmp.Diff(want, r.Logs, ignore); diff != "" {
						t.Fatalf("receipt[%d] logs diff (-want +got):\n%s", i, diff)
					}
					if diff := cmp.Diff(*want[0], <-logEvents, ignore); diff != "" {
						t.Fatalf("Log subscription receive [%d] diff (-want +got):\n%s", i, diff)
					}
				}

				close(logEvents)
				for extra := range logEvents {
					t.Errorf("Log subscription received extraneous %T: %[1]v", extra)
				}
			})
		})

		t.Run("subscriptions", func(t *testing.T) {
			t.Run("head_events", func(t *testing.T) {
				for n := len(acceptedBlocks); !acceptedBlocks[n-1].executed.Load(); {
					// There's no need to block on execution in production so
					// making the executed bool a channel is overkill.
					runtime.Gosched()
				}

				headSub.Unsubscribe()
				require.NoErrorf(t, <-headSub.Err(), "receive on %T.Err()", headSub)
				close(headEvents)

				var got []common.Hash
				for hdr := range headEvents {
					got = append(got, hdr.Hash())
				}
				var want []common.Hash
				for _, b := range acceptedBlocks {
					want = append(want, b.Hash())
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("Chain-head subscription: block hashes diff (-want +got):\n%s", diff)
				}
			})
		})
	})

	t.Run("accounts_and_storage", func(t *testing.T) {
		want := uint64(len(allTxs))
		// TODO(arr4n) iterate over all accepted blocks, using the cumulative tx
		// count as expected value when performing the below tests at different
		// block numbers.

		gotNonce, err := rpcClient.NonceAt(ctx, eoa, nil)
		require.NoErrorf(t, err, "%T.NonceAt(%v, [latest])", rpcClient, eoa)
		assert.Equal(t, want, gotNonce, "Nonce of EOA sending txs")

		contract, err := weth.NewWeth(wethAddr, rpcClient)
		require.NoError(t, err, "bind WETH contract")
		gotWrapped, err := contract.BalanceOf(nil, eoa)
		require.NoErrorf(t, err, "%T.BalanceOf(%v)", contract, eoa)
		require.Truef(t, gotWrapped.IsUint64(), "%T.BalanceOf(%v).IsUint64()", contract, eoa)
		assert.Equal(t, want, gotWrapped.Uint64(), "%T.BalanceOf(%v)", contract, eoa)

		gotContractBal, err := rpcClient.BalanceAt(ctx, wethAddr, nil)
		require.NoError(t, err)
		require.Truef(t, gotContractBal.IsUint64(), "")
		assert.Equal(t, want, gotContractBal.Uint64(), "Balance of WETH contract")
	})

	t.Run("recover_from_db", func(t *testing.T) {
		genesisID, err := vm.GetBlockIDAtHeight(ctx, 0)
		require.NoError(t, err, "fetch genesis ID from original VM")

		recovered, err := New(
			ctx,
			Config{
				LastSynchronousBlock: common.Hash(genesisID),
				DB:                   vm.db,
				SnowCtx:              snowCtx,
				ChainConfig:          chainConfig,
			},
		)
		require.NoErrorf(t, err, "New(%T[DB from original VM])", Config{})

		if diff := cmp.Diff(vm, recovered, cmpVMs(ctx, t)); diff != "" {
			t.Errorf("%T from used DB diff (-recovered +original):\n%s", vm, diff)
		}
		require.NoErrorf(t, recovered.Shutdown(ctx), "%T.Shutdown()", recovered)
	})

	require.NoErrorf(t, vm.Shutdown(ctx), "%T.Shutdown()", vm)
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

func cmpBigInts() cmp.Options {
	return cmp.Options{
		cmp.Comparer(func(a, b *big.Int) bool {
			if a == nil || b == nil {
				return a == nil && b == nil
			}
			return a.Cmp(b) == 0
		}),
	}
}

func cmpTimes() cmp.Options {
	return cmp.Options{
		cmp.AllowUnexported(
			gastime.TimeMarshaler{},
			proxytime.Time[gas.Gas]{},
		),
		cmpopts.IgnoreFields(gastime.TimeMarshaler{}, "canotoData"),
		cmpopts.IgnoreFields(proxytime.Time[gas.Gas]{}, "canotoData"),
	}
}

func cmpBlocks() cmp.Options {
	return cmp.Options{
		cmpBigInts(),
		cmpTimes(),
		cmp.AllowUnexported(
			Block{},
			executionResults{},
		),
		cmp.Comparer(func(a, b *types.Block) bool {
			return a.Hash() == b.Hash()
		}),
		cmp.Comparer(func(a, b types.Receipts) bool {
			return types.DeriveSha(a, trieHasher()) == types.DeriveSha(b, trieHasher())
		}),
		cmpopts.EquateComparable(
			// We're not running tests concurrently with anything that will
			// modify [Block.accepted] nor [Block.executed] so this is safe.
			// Using a [cmp.Transformer] would make the linter complain about
			// copying.
			atomic.Bool{},
		),
		cmpopts.IgnoreTypes(
			canotoData_executionResults{},
		),
		cmpopts.IgnoreFields(
			executionResults{},
			"byTime", // wall-clock for metrics only
		),
	}
}

func cmpVMs(ctx context.Context, tb testing.TB) cmp.Options {
	var (
		zeroVM   VM
		zeroExec executor
	)

	return cmp.Options{
		cmpBlocks(),
		cmp.AllowUnexported(
			VM{},
			last{},
			executor{},
			executionScratchSpace{},
		),
		cmpopts.IgnoreUnexported(params.ChainConfig{}),
		cmpopts.IgnoreFields(VM{}, "preference"),
		cmpopts.IgnoreTypes(
			zeroVM.snowCtx,
			zeroVM.mempool,
			zeroExec.queue,
			&snapshot.Tree{},
		),
		cmpopts.IgnoreInterfaces(struct{ snowcommon.AppHandler }{}),
		cmpopts.IgnoreInterfaces(struct{ ethdb.Database }{}),
		cmpopts.IgnoreInterfaces(struct{ logging.Logger }{}),
		cmpopts.IgnoreInterfaces(struct{ state.Database }{}),
		cmp.Transformer("block_map_mu", func(mu sink.Mutex[blockMap]) blockMap {
			bm, err := sink.FromMutex(ctx, mu, func(bm blockMap) (blockMap, error) {
				return bm, nil
			})
			require.NoError(tb, err)
			return bm
		}),
		cmp.Transformer("atomic_block", func(p atomic.Pointer[Block]) *Block {
			return p.Load()
		}),
		cmp.Transformer("state_dump", func(db *state.StateDB) state.Dump {
			return db.RawDump(&state.DumpConfig{})
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
