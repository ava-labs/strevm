package sae

import (
	"context"
	"errors"
	"flag"
	"math"
	"math/big"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook/hooktest"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/weth"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	txsInIntegrationTest = flag.Uint64("wrap_avax_tx_count", 1_000, "Number of transactions to use in TestIntegrationWrapAVAX")
	cpuProfileDest       = flag.String("cpu_profile_out", "", "If non-empty, file to which pprof CPU profile is written")
)

func TestIntegrationWrapAVAX(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	t.Logf("Sending all txs as EOA %v", eoa)
	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)

	now := time.Unix(0, 0)

	vm, snowCompatVM := newVM(
		ctx, t,
		func() time.Time {
			return now
		},
		hooktest.Simple{
			T: 2e6,
		},
		tbLogger{tb: t, level: logging.Debug + 1},
		genesisJSON(t, chainConfig, eoa),
	)

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
		assert.Nil(t, block.ParentBlock(), "parent")
		assert.Nil(t, block.LastSettled(), "last settled")
		assert.True(t, block.Executed(), "executed")

		for k, ptr := range map[string]*atomic.Pointer[blocks.Block]{
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

	allTxs := make([]*types.Transaction, *txsInIntegrationTest)
	for nonce := range *txsInIntegrationTest {
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
		acceptedBlocks      []*blocks.Block
		blockWithLastTx     *blocks.Block
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

		bb := unwrapBlock(t, b)
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
		} else if s := bb.LastSettled(); s != nil && s.NumberU64() >= blockWithLastTx.NumberU64() {
			t.Logf("Built and accepted %s blocks in %v", human(numBlocks), time.Since(start))
			t.Logf("Consensus waited for execution %s times", human(waitingForExecution))
			break
		}
	}

	t.Cleanup(func() {
		// See block pruning at the end of [VM.AcceptBlock]
		n := blocks.InMemoryBlockCount()
		acceptedBlocks = nil
		blockWithLastTx = nil
		runtime.GC()
		t.Logf("After GC, in-memory block count: %d => %d", n, blocks.InMemoryBlockCount())
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
	require.Eventuallyf(t, lastBlock.Executed, time.Second, 10*time.Millisecond, "executed.Load() on last %T", lastBlock)

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
			assert.Equalf(t, b.PostExecutionStateRoot(), lastBlock.Root(), "%T.Root() of last block must be post-execution root of block including last tx", lastBlock)

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
			lastExecutedBy := blockWithLastTx.ExecutedByWallTime()
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
					fromDB := rawdb.ReadReceipts(vm.db, b.Hash(), b.NumberU64(), b.Time(), vm.exec.ChainConfig())

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
				require.NoError(t, acceptedBlocks[len(acceptedBlocks)-1].WaitUntilExecuted(ctx))

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
				SnowCtx:              vm.snowCtx,
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
