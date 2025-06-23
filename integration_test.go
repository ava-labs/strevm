package sae

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/weth"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	txsInIntegrationTest = uint64s{10, 30, 100, 300, 1000, 3000}
	// Disabling minimum gas consumption isn't useful for testing, but it
	// distorts gas per second so may be used for (relative) throughput testing.
	enableMinGasConsumption = flag.Bool("enable_min_gas_consumption", true, "Enforce lambda lower bound on gas consumption in integration test")
)

func init() {
	flag.Var(&txsInIntegrationTest, "wrap_avax_tx_count", "Number of transactions to use in TestIntegrationWrapAVAX (comma-separated)")
}

type stubHooks struct {
	T gas.Gas
}

func (h *stubHooks) GasTarget(parent *types.Block) gas.Gas {
	return h.T
}

func TestIntegrationWrapAVAX(t *testing.T) {
	for _, n := range txsInIntegrationTest {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			testIntegrationWrapAVAX(t, n)
		})
	}
}

func testIntegrationWrapAVAX(t *testing.T, numTxsInTest uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	key := newTestPrivateKey(t, nil)
	eoa := crypto.PubkeyToAddress(key.PublicKey)
	t.Logf("Sending all txs as EOA %v", eoa)

	chainConfig := params.TestChainConfig
	signer := types.LatestSigner(chainConfig)
	const genesisTime = 1600858926
	now := time.Unix(genesisTime, 0)

	setTrieDBCommitBlockIntervalLog2(t, 2)

	vm := newVM(
		ctx, t,
		func() time.Time {
			return now
		},
		&stubHooks{
			T: 2e6,
		},
		tbLogger{tb: t, level: logging.Debug + 1},
		genesisJSON(t, genesisTime, chainConfig, eoa),
	)

	if *enableMinGasConsumption {
		saetest.EnableMinimumGasConsumption(t)
	}

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
			if diff := cmp.Diff(block, ptr.Load(), blocks.CmpOpt()); diff != "" {
				t.Errorf("%T.last.%s diff (-want +got):\n%s", vm, k, diff)
			}
		}
	})

	allTxs := make([]*types.Transaction, numTxsInTest)
	// Tx gas limit high enough that [hook.MinimumGasConsumption] is enforced.
	const (
		commonTxGasLimit  = 499_999
		minGasConsumption = 250_000 // rounded *up*
	)
	for nonce := range numTxsInTest {
		allTxs[nonce] = types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			Nonce:     nonce,
			To:        &wethAddr,
			Value:     big.NewInt(1),
			Gas:       commonTxGasLimit,
			GasTipCap: big.NewInt(0),
			GasFeeCap: new(big.Int).SetUint64(math.MaxUint64),
		})
		require.NoError(t, vm.rpc.SendTransaction(ctx, allTxs[nonce]))
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

	headEvents := make(chan *types.Header, len(allTxs))
	headSub, err := vm.rpc.SubscribeNewHead(ctx, headEvents)
	require.NoErrorf(t, err, "%T.SubscribeNewHead()", vm.rpc)

	filter := ethereum.FilterQuery{Addresses: []common.Address{wethAddr}}
	logEvents := make(chan types.Log, len(allTxs))
	logSub, err := vm.rpc.SubscribeFilterLogs(ctx, filter, logEvents)
	require.NoErrorf(t, err, "%T.SubscribeFilterLogs()", err)

	var (
		acceptedBlocks      []*blocks.Block
		blockWithLastTx     *blocks.Block
		waitingForExecution int
	)
	inclusion := make(map[common.Hash]txInclusion)
	start := time.Now()
	for numBlocks := 0; ; numBlocks++ {
		// This loop fakes consensus so only allow access to the Snow-compatible
		// VM view. It's not a guarantee of correct behaviour, but it does
		// ensure that we don't accidentally use the raw [VM].
		var vm block.ChainVM = vm.snow

		lastID, err := vm.LastAccepted(ctx)
		require.NoErrorf(t, err, "%T.LastAccepted()", vm)
		require.NoErrorf(t, vm.SetPreference(ctx, lastID), "%T.SetPreference(LastAccepted())", vm)

		proposed, err := vm.BuildBlock(ctx)
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
		require.NoErrorf(t, err, "%T.BuildBlock() #%d", vm, numBlocks)

		b, err := vm.ParseBlock(ctx, proposed.Bytes())
		require.NoErrorf(t, err, "%T.ParseBlock()", vm)
		require.Equal(t, now.Truncate(time.Second), b.Timestamp())

		require.NoErrorf(t, b.Verify(ctx), "%T.Verify()", b)
		require.NoErrorf(t, b.Accept(ctx), "%T.Accept()", b)

		rawB := unwrapBlock(t, b)
		acceptedBlocks = append(acceptedBlocks, rawB)
		for i, tx := range rawB.Transactions() {
			inclusion[tx.Hash()] = txInclusion{
				blockHash: rawB.Hash(),
				blockNum:  rawB.Height(),
				txIndex:   uint(i),
			}
		}

		now = now.Add(900 * time.Millisecond)

		n, err := numPendingTxs(ctx)
		require.NoError(t, err, "inspect mempool for num pending txs")
		if n > 0 {
			continue
		}
		if blockWithLastTx == nil {
			blockWithLastTx = rawB
			t.Logf("Last tx included in block %d", rawB.NumberU64())
		} else if s := rawB.LastSettled(); s != nil && s.NumberU64() >= blockWithLastTx.NumberU64() {
			t.Logf("Built and accepted %s blocks in %v", human(numBlocks), time.Since(start))
			t.Logf("Consensus waited for execution %s times", human(waitingForExecution))
			break
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	t.Cleanup(func() {
		// See block pruning at the end of [VM.AcceptBlock]
		n := blocks.InMemoryBlockCount()
		acceptedBlocks = nil
		blockWithLastTx = nil
		runtime.GC()
		t.Logf("After GC, in-memory block count: %d => %d", n, blocks.InMemoryBlockCount())
	})

	ctx = context.Background() // original ctx had a timeout in case of execution deadlock, but we're past that

	lastID, err := vm.LastAccepted(ctx)
	require.NoErrorf(t, err, "%T.LastAccepted()", vm)
	lastBlock, err := vm.GetBlock(ctx, lastID)
	require.NoErrorf(t, err, "%T.GetBlock(LastAccepted())", vm)
	require.NoErrorf(t, lastBlock.WaitUntilExecuted(ctx), "%T{last}.WaitUntilExecuted()", lastBlock)

	t.Run("persisted_blocks", func(t *testing.T) {
		t.Parallel()
		t.Run("with_last_tx", func(t *testing.T) {
			b := blockWithLastTx

			t.Run("last_settled_block", func(t *testing.T) {
				safe := rpc.SafeBlockNumber
				settled, err := vm.rpc.HeaderByNumber(ctx, big.NewInt(safe.Int64()))
				require.NoErrorf(t, err, "%T.HeaderByNumber(%d %q)", vm.rpc, safe, safe)

				assert.GreaterOrEqual(t, settled.Number.Cmp(b.Number()), 0, "last-settled height >= block including last tx")
			})
			assert.Equalf(t, b.PostExecutionStateRoot(), lastBlock.SettledStateRoot(), "%T.SettledStateRoot() of last block must be post-execution root of block including last tx", lastBlock)

			t.Run("rawdb_tx_lookup_entries", func(t *testing.T) {
				lastTxHash := allTxs[len(allTxs)-1].Hash()
				got, err := vm.rpc.TransactionReceipt(ctx, lastTxHash)
				require.NoErrorf(t, err, "%T.TransactionReceipt(%#x [last tx])", vm.rpc, lastTxHash)

				assert.Equal(t, b.Hash(), got.BlockHash, "block hash of last tx")
				assert.Equal(t, uint(len(b.Transactions())-1), got.TransactionIndex, "index of last tx")
			})
		})

		_, got := rawdb.ReadAllCanonicalHashes(vm.db, 1, math.MaxUint64, math.MaxInt)
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
				rs, err := vm.rpc.BlockReceipts(ctx, num)
				require.NoErrorf(t, err, "%T.BlockReceipts(%d)", vm.rpc, i)
				gotReceipts = append(gotReceipts, rs)

				for _, r := range rs {
					totalGasConsumed += gas.Gas(r.GasUsed)
				}
			}
			gotReceiptsConcat := slices.Concat(gotReceipts...)

			require.Equalf(t, len(allTxs), len(gotReceiptsConcat), "# %T == # %T", &types.Receipt{}, &types.Transaction{})
			lastExecutedBy := blockWithLastTx.ExecutedByWallTime()
			t.Logf("Executed %s txs in %v", human(len(gotReceiptsConcat)), lastExecutedBy.Sub(start))
			if !*enableMinGasConsumption {
				t.Logf("%s gas consumed", human(totalGasConsumed))
			}

			t.Run("vs_sent_txs", func(t *testing.T) {
				t.Parallel()

				const wantGasUsed = minGasConsumption
				var (
					lastBlockNum, wantCumulGasUsed uint64
					wantReceipts                   types.Receipts
				)
				for _, tx := range allTxs {
					inc := inclusion[tx.Hash()]
					if inc.blockNum != lastBlockNum {
						lastBlockNum = inc.blockNum
						wantCumulGasUsed = wantGasUsed
					} else {
						wantCumulGasUsed += wantGasUsed
					}

					wantReceipts = append(wantReceipts, &types.Receipt{
						Type:              types.DynamicFeeTxType,
						TxHash:            tx.Hash(),
						Status:            1,
						GasUsed:           wantGasUsed,
						CumulativeGasUsed: wantCumulGasUsed,
						BlockHash:         inc.blockHash,
						BlockNumber:       new(big.Int).SetUint64(inc.blockNum),
						TransactionIndex:  inc.txIndex,
					})
				}
				ignore := []string{
					"Logs", // checked below
					"EffectiveGasPrice",
					"Bloom",
				}
				if !*enableMinGasConsumption {
					ignore = append(ignore, "GasUsed", "CumulativeGasUsed")
				}
				opt := cmpopts.IgnoreFields(types.Receipt{}, ignore...)
				if diff := cmp.Diff(wantReceipts, gotReceiptsConcat, opt, saetest.CmpBigInts()); diff != "" {
					t.Errorf("Execution results diff (-want +got): \n%s", diff)
				}
			})

			t.Run("vs_direct_db_read", func(t *testing.T) {
				t.Parallel()

				for i, b := range acceptedBlocks {
					fromAPI := gotReceipts[i+1 /*skips genesis*/]
					fromDB := rawdb.ReadReceipts(vm.db, b.Hash(), b.NumberU64(), b.Time(), vm.exec.ChainConfig())

					if diff := cmp.Diff(fromDB, fromAPI, saetest.CmpBigInts()); diff != "" {
						t.Errorf("Block %d receipts diff vs db: -rawdb.ReadReceipts() +%T.BlockReceipts():\n%s", b.NumberU64(), vm.rpc, diff)
					}
					if diff := cmp.Diff(txHashes(b.Transactions()), txHashes(fromAPI)); diff != "" {
						t.Errorf("Block %d receipt hashes diff vs proposed block: -%T.Transactions() +%T.BlockReceipts():\n%s", b.NumberU64(), b, vm.rpc, diff)
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
				want := []*types.Log{{
					Address: wethAddr,
					Topics: []common.Hash{
						crypto.Keccak256Hash([]byte("Deposit(address,uint256)")),
						eoaAsHash,
					},
					Data: uint256.NewInt(1).PaddedBytes(32),
					// TxHash and inclusion data to be completed for each.
				}}

				logSub.Unsubscribe()
				require.NoErrorf(t, <-logSub.Err(), "receive on %T.Err()", logSub)

				var (
					lastBlockNum uint64
					index        uint
				)
				for i, r := range slices.Concat(gotReceipts...) {
					inc := inclusion[r.TxHash]
					if inc.blockNum != lastBlockNum {
						lastBlockNum = inc.blockNum
						index = 0
					} else {
						index++
					}

					want[0].TxHash = r.TxHash
					want[0].BlockHash = inc.blockHash
					want[0].BlockNumber = inc.blockNum
					want[0].TxIndex = inc.txIndex
					want[0].Index = index

					// Use `t.Fatalf()` because all receipts should be the same;
					// if one fails then they'll all probably fail in the same
					// way and there's no point being inundated with thousands
					// of identical errors.
					if diff := cmp.Diff(want, r.Logs); diff != "" {
						t.Fatalf("receipt[%d] logs diff (-want +got):\n%s", i, diff)
					}
					if diff := cmp.Diff(*want[0], <-logEvents); diff != "" {
						t.Fatalf("Log subscription receive [%d] diff (-want +got):\n%s", i, diff)
					}
				}

				close(logEvents)
				for extra := range logEvents {
					t.Errorf("Log subscription received extraneous %T: %[1]v", extra)
				}
			})
		})

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

	t.Run("accounts_and_storage", func(t *testing.T) {
		want := uint64(len(allTxs))
		// TODO(arr4n) iterate over all accepted blocks, using the cumulative tx
		// count as expected value when performing the below tests at different
		// block numbers.

		gotNonce, err := vm.rpc.NonceAt(ctx, eoa, nil)
		require.NoErrorf(t, err, "%T.NonceAt(%v, [latest])", vm.rpc, eoa)
		assert.Equal(t, want, gotNonce, "Nonce of EOA sending txs")

		contract, err := weth.NewWeth(wethAddr, vm.rpc)
		require.NoError(t, err, "bind WETH contract")
		gotWrapped, err := contract.BalanceOf(nil, eoa)
		require.NoErrorf(t, err, "%T.BalanceOf(%v)", contract, eoa)
		require.Truef(t, gotWrapped.IsUint64(), "%T.BalanceOf(%v).IsUint64()", contract, eoa)
		assert.Equal(t, want, gotWrapped.Uint64(), "%T.BalanceOf(%v)", contract, eoa)

		gotContractBal, err := vm.rpc.BalanceAt(ctx, wethAddr, nil)
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
				Hooks:       vm.hooks,
				ChainConfig: vm.exec.ChainConfig(),
				DB:          vm.db,
				LastSynchronousBlock: LastSynchronousBlock{
					Hash:   common.Hash(genesisID),
					Target: genesisBlockGasTarget,
				},
				SnowCtx: vm.snowCtx,
			},
		)
		require.NoErrorf(t, err, "New(%T[DB from original VM])", Config{})

		if diff := cmp.Diff(vm.VM, recovered, cmpVMs(ctx, t)); diff != "" {
			t.Errorf("%T from DB recovery diff (-recovered +original):\n%s", vm, diff)
		}
		if diff := cmp.Diff(vm.exec, recovered.exec, saexec.CmpOpt(ctx)); diff != "" {
			t.Errorf("%T from DB recovery diff (-recovered +original):\n%s", vm.exec, diff)
		}
		require.NoErrorf(t, recovered.Shutdown(ctx), "%T.Shutdown()", recovered)

		// TODO(arr4n) execute one more tx on both the original and the
		// recovered VMs. Ensure that the gas price is >1 as this is a good test
		// of the excess.
	})

	require.NoErrorf(t, vm.Shutdown(ctx), "%T.Shutdown()", vm)
}
