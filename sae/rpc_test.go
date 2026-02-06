// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/ethapi"
	libevmhookstest "github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/cmputils"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest"
)

type rpcTest struct {
	method string
	args   []any
	want   any
}

func (s *SUT) testRPC(ctx context.Context, t *testing.T, tcs ...rpcTest) {
	t.Helper()
	opts := []cmp.Option{
		cmpopts.EquateEmpty(),
		cmputils.Headers(),
		cmputils.HexutilBigs(),
		cmputils.TransactionsByHash(),
	}

	for _, tc := range tcs {
		t.Run(tc.method, func(t *testing.T) {
			got := reflect.New(reflect.TypeOf(tc.want))
			t.Logf("%T.CallContext(ctx, %T, %q, %v...)", s.rpcClient, &tc.want /*i.e. the type*/, tc.method, tc.args)
			require.NoError(t, s.CallContext(ctx, got.Interface(), tc.method, tc.args...))
			if diff := cmp.Diff(tc.want, got.Elem().Interface(), opts...); diff != "" {
				t.Errorf("Diff (-want +got):\n%s", diff)
			}
		})
	}
}

// testRPCGetter allows testing of RPC methods for which the return types are
// difficult to unmarshal but there exists a helper such as [ethclient.Client].
// [SUT.testRPC] SHOULD be preferred.
func testRPCGetter[
	Arg any,
	T interface {
		// Only add extra types if JSON unmarshalling from RPC methods directly
		// into the type will fail.
		*types.Block
	},
](ctx context.Context, t *testing.T, underlyingRPCMethod string, get func(context.Context, Arg) (T, error), arg Arg, want T) {
	t.Helper()
	opts := []cmp.Option{
		cmputils.Blocks(),
		cmputils.Headers(),
		cmpopts.EquateEmpty(),
	}

	t.Run(underlyingRPCMethod, func(t *testing.T) {
		got, err := get(ctx, arg)
		t.Logf("%s(%v)", underlyingRPCMethod, arg)
		require.NoErrorf(t, err, "%s(%v)", underlyingRPCMethod, arg)
		if diff := cmp.Diff(want, got, opts...); diff != "" {
			t.Errorf("%s(%v) diff (-want +got):\n%s", underlyingRPCMethod, arg, diff)
		}
	})
}

func TestSubscriptions(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	newHeads := make(chan *types.Header, 1)
	sub, err := sut.SubscribeNewHead(ctx, newHeads)
	require.NoError(t, err, "SubscribeNewHead(...)")
	// The subscription is closed in a defer rather than via t.Cleanup to ensure
	// that is is closed before the rest of the SUT is torn down. Otherwise,
	// there could be a goroutine leak.
	defer sub.Unsubscribe()

	b := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
	got := <-newHeads
	require.Equalf(t, b.Hash(), got.Hash(), "%T.Hash() from %T.SubscribeNewHead(...)", got, sut.Client)
}

func TestWeb3Namespace(t *testing.T) {
	var (
		preImage hexutil.Bytes = []byte("test")
		digest                 = hexutil.Bytes(crypto.Keccak256(preImage))
	)

	ctx, sut := newSUT(t, 1)
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "web3_clientVersion",
			want:   version.GetVersions().String(),
		},
		{
			method: "web3_sha3",
			args:   []any{preImage},
			want:   digest,
		},
	}...)
}

func TestNetNamespace(t *testing.T) {
	testRPCMethodsWithPeers := func(sut *SUT, wantPeerCount hexutil.Uint) {
		t.Helper()
		sut.testRPC(sut.context(t), t, []rpcTest{
			{
				method: "net_listening",
				want:   true,
			},
			{
				method: "net_peerCount",
				want:   wantPeerCount,
			},
			{
				method: "net_version",
				want:   fmt.Sprintf("%d", saetest.ChainConfig().ChainID.Uint64()),
			},
		}...)
	}

	_, sut := newSUT(t, 1) // No peers
	testRPCMethodsWithPeers(sut, 0)

	const (
		numValidators    = 1
		numNonValidators = 2
	)
	n := newNetworkedSUTs(t, numValidators, numNonValidators)
	for _, sut := range n.validators {
		testRPCMethodsWithPeers(sut, numValidators+numNonValidators-1)
	}
	for _, sut := range n.nonValidators {
		testRPCMethodsWithPeers(sut, numValidators)
	}
}

func TestTxPoolNamespace(t *testing.T) {
	ctx, sut := newSUT(t, 2)

	addresses := sut.wallet.Addresses()
	makeTx := func(i int) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, i, &types.DynamicFeeTx{
			To:        &addresses[i],
			Gas:       params.TxGas + uint64(i), //nolint:gosec // Won't overflow
			GasFeeCap: big.NewInt(int64(i + 1)),
			Value:     big.NewInt(int64(i + 10)),
		})
	}

	const (
		pendingAccount = 0
		queuedAccount  = 1
	)
	pendingTx := makeTx(pendingAccount)
	pendingRPCTx := ethapi.NewRPCPendingTransaction(pendingTx, nil, saetest.ChainConfig())

	_ = makeTx(queuedAccount) // skip the nonce to gap the mempool
	queuedTx := makeTx(queuedAccount)
	queuedRPCTx := ethapi.NewRPCPendingTransaction(queuedTx, nil, saetest.ChainConfig())

	sut.mustSendTx(t, pendingTx)
	sut.mustSendTx(t, queuedTx)
	sut.syncMempool(t)

	// TODO: This formatting is copied from libevm, consider exposing it somehow
	// or removing the dependency on the exact format.
	txToSummary := func(tx *types.Transaction) string {
		return fmt.Sprintf("%s: %d wei + %d gas × %d wei",
			tx.To(),
			tx.Value().Uint64(),
			tx.Gas(),
			tx.GasPrice(),
		)
	}

	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "txpool_content",
			want: map[string]map[string]map[string]*ethapi.RPCTransaction{
				"pending": {
					addresses[pendingAccount].Hex(): {
						"0": pendingRPCTx,
					},
				},
				"queued": {
					addresses[queuedAccount].Hex(): {
						"1": queuedRPCTx,
					},
				},
			},
		},
		{
			method: "txpool_contentFrom",
			args:   []any{addresses[pendingAccount]},
			want: map[string]map[string]*ethapi.RPCTransaction{
				"pending": {
					"0": pendingRPCTx,
				},
				"queued": {},
			},
		},
		{
			method: "txpool_contentFrom",
			args:   []any{addresses[queuedAccount]},
			want: map[string]map[string]*ethapi.RPCTransaction{
				"pending": {},
				"queued": {
					"1": queuedRPCTx,
				},
			},
		},
		{
			method: "txpool_inspect",
			want: map[string]map[string]map[string]string{
				"pending": {
					addresses[pendingAccount].Hex(): {
						"0": txToSummary(pendingTx),
					},
				},
				"queued": {
					addresses[queuedAccount].Hex(): {
						"1": txToSummary(queuedTx),
					},
				},
			},
		},
		{
			method: "txpool_status",
			want: map[string]hexutil.Uint{
				"pending": 1,
				"queued":  1,
			},
		},
	}...)
}

func TestEthSyncing(t *testing.T) {
	ctx, sut := newSUT(t, 1)
	// Avalanchego does not expose APIs until after the node has fully synced,
	// so eth_syncing always returns false (not syncing).
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_syncing",
		want:   false,
	})
}

func TestChainID(t *testing.T) {
	for id := range uint64(2) {
		ctx, sut := newSUT(t, 0, options.Func[sutConfig](func(c *sutConfig) {
			c.genesis.Config = &params.ChainConfig{
				ChainID: new(big.Int).SetUint64(id),
			}
		}))
		sut.testRPC(ctx, t, rpcTest{
			method: "eth_chainId",
			want:   hexutil.Uint64(id),
		})
	}
}

func TestEthGetters(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))

	ctx, sut := newSUT(t, 1, opt)

	t.Run("unknown_hashes", func(t *testing.T) {
		sut.testGetByUnknownHash(ctx, t)
	})
	t.Run("unknown_numbers", func(t *testing.T) {
		sut.testGetByUnknownNumber(ctx, t)
	})

	// The named block "pending" is the last to be enqueued but yet to be
	// executed. Although unlikely to be useful in practice, it still needs to
	// be tested.
	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	unblockPrecompile := make(chan struct{})
	t.Cleanup(func() { close(unblockPrecompile) })

	libevmHooks := &libevmhookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			blockingPrecompile: vm.NewStatefulPrecompile(func(vm.PrecompileEnvironment, []byte) ([]byte, error) {
				<-unblockPrecompile
				return nil, nil
			}),
		},
	}
	libevmHooks.Register(t)

	createTx := func(t *testing.T, to common.Address) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &to,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}
	var zeroAddr common.Address

	genesis := sut.lastAcceptedBlock(t)

	// Once a block is settled, its ancestors are only accessible from the
	// database.
	onDisk := sut.createAndAcceptBlock(t, createTx(t, zeroAddr))

	settled := sut.createAndAcceptBlock(t, createTx(t, zeroAddr))
	require.NoErrorf(t, settled.WaitUntilExecuted(ctx), "%T.WaitUntilSettled()", settled)
	vmTime.set(settled.ExecutedByGasTime().AsTime().Add(saeparams.Tau)) // ensure it will be settled

	executed := sut.createAndAcceptBlock(t, createTx(t, zeroAddr))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	pending := sut.createAndAcceptBlock(t, createTx(t, blockingPrecompile))

	for _, b := range []*blocks.Block{genesis, onDisk, settled, executed, pending} {
		t.Run(fmt.Sprintf("block_num_%d", b.Height()), func(t *testing.T) {
			ethB := b.EthBlock()
			sut.testGetByHash(ctx, t, ethB)
			sut.testGetByNumber(ctx, t, ethB, rpc.BlockNumber(b.Number().Int64()))
		})
	}

	t.Run("named_blocks", func(t *testing.T) {
		// [ethclient.Client.BlockByNumber] isn't compatible with pending blocks as
		// the geth RPC server strips fields that the client then expects to find.
		sut.testRPC(ctx, t, rpcTest{
			method: "eth_getHeaderByNumber",
			args:   []any{rpc.PendingBlockNumber},
			want:   pending.Header(),
		})

		tests := []struct {
			num  rpc.BlockNumber
			want *blocks.Block
		}{
			{rpc.LatestBlockNumber, executed},
			{rpc.SafeBlockNumber, settled},
			{rpc.FinalizedBlockNumber, settled},
		}

		for _, tt := range tests {
			t.Run(tt.num.String(), func(t *testing.T) {
				sut.testGetByNumber(ctx, t, tt.want.EthBlock(), tt.num)
			})
		}

		sut.testRPC(ctx, t, rpcTest{
			method: "eth_blockNumber",
			want:   hexutil.Uint64(executed.Height()),
		})
	})
}

func TestReceiptAPIs(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 5, opt)

	// Blocking precompile creates accepted-but-not-executed blocks
	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	unblockPrecompile := make(chan struct{})
	t.Cleanup(func() { close(unblockPrecompile) })

	libevmHooks := &libevmhookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			blockingPrecompile: vm.NewStatefulPrecompile(func(vm.PrecompileEnvironment, []byte) ([]byte, error) {
				<-unblockPrecompile
				return nil, nil
			}),
		},
	}
	libevmHooks.Register(t)

	createTx := func(t *testing.T, account int, to common.Address) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, account, &types.DynamicFeeTx{
			To:        &to,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}
	getReceipt := func(t *testing.T, tx *types.Transaction) *types.Receipt {
		t.Helper()
		var receipt *types.Receipt
		require.NoError(t, sut.CallContext(ctx, &receipt, "eth_getTransactionReceipt", tx.Hash()))
		require.NotNil(t, receipt, "receipt for tx %s", tx.Hash())
		return receipt
	}

	var zeroAddr common.Address

	// All blocks created upfront to avoid executor blocking during subtests.

	genesis := sut.lastAcceptedBlock(t)

	// Block 1: Executed, still in cache (not yet settled)
	txExecutedInCache := createTx(t, 0, zeroAddr)
	blockExecutedInCache := sut.createAndAcceptBlock(t, txExecutedInCache)
	require.NoErrorf(t, blockExecutedInCache.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", blockExecutedInCache)

	// Block 2: Will be settled to DB
	txSettled := createTx(t, 1, zeroAddr)
	blockSettled := sut.createAndAcceptBlock(t, txSettled)
	require.NoErrorf(t, blockSettled.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", blockSettled)
	receiptFromCache := getReceipt(t, txSettled)
	vmTime.set(blockSettled.ExecutedByGasTime().AsTime().Add(saeparams.Tau))

	// Block 3: Multiple txs, executed in cache
	txMulti1 := createTx(t, 2, zeroAddr)
	txMulti2 := createTx(t, 2, zeroAddr)
	txMulti3 := createTx(t, 2, zeroAddr)
	blockMultiTxs := sut.createAndAcceptBlock(t, txMulti1, txMulti2, txMulti3)
	require.NoErrorf(t, blockMultiTxs.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", blockMultiTxs)

	// Block 4: Triggers settlement of block 2
	triggerSettlement := createTx(t, 3, zeroAddr)
	b4 := sut.createAndAcceptBlock(t, triggerSettlement)
	require.NoErrorf(t, b4.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b4)
	require.NoErrorf(t, blockSettled.WaitUntilSettled(ctx), "%T.WaitUntilSettled()", blockSettled)
	sut.rawVM.blocks.Delete(blockSettled.Hash())
	require.NotNilf(t, rawdb.ReadHeaderNumber(sut.db, blockSettled.Hash()), "db header for block %d", blockSettled.Height())
	receiptFromDB := getReceipt(t, txSettled)

	// Block 5: Accepted but not executed (must be last to avoid blocking)
	txPending := createTx(t, 4, blockingPrecompile)
	_ = sut.createAndAcceptBlock(t, txPending)

	// shoutout austin larson
	if diff := cmp.Diff(receiptFromCache, receiptFromDB, cmputils.Receipts()); diff != "" {
		t.Fatalf("cache vs db receipt diff (-cache +db):\n%s", diff)
	}

	t.Run("eth_getTransactionReceipt", func(t *testing.T) {
		// Receipts found via cache and DB code paths
		for _, tc := range []struct {
			name string
			tx   *types.Transaction
		}{
			{
				"executed_in_cache",
				txExecutedInCache,
			},
			{
				"settled_in_db",
				txSettled,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				receipt := getReceipt(t, tc.tx)
				require.Equal(t, tc.tx.Hash(), receipt.TxHash)
				require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
			})
		}

		// Nil returns: not-yet-executed and nonexistent transactions
		sut.testRPC(ctx, t, []rpcTest{
			{
				method: "eth_getTransactionReceipt",
				args:   []any{txPending.Hash()},
				want:   (*types.Receipt)(nil),
			},
			{
				method: "eth_getTransactionReceipt",
				args:   []any{common.Hash{}},
				want:   (*types.Receipt)(nil),
			},
		}...)
	})

	requireBlockReceipts := func(t *testing.T, blockID any, txs []*types.Transaction) {
		t.Helper()
		var got []*types.Receipt
		require.NoError(t, sut.CallContext(ctx, &got, "eth_getBlockReceipts", blockID))
		require.Len(t, got, len(txs))
		for i, r := range got {
			require.Equal(t, txs[i].Hash(), r.TxHash, "receipt[%d]", i)
			require.Equal(t, uint(i), r.TransactionIndex, "receipt[%d]", i) //nolint:gosec // test index
			require.Equal(t, types.ReceiptStatusSuccessful, r.Status, "receipt[%d]", i)
		}
	}

	t.Run("eth_getBlockReceipts", func(t *testing.T) {
		multiTxs := []*types.Transaction{txMulti1, txMulti2, txMulti3}

		// Receipts found via cache and DB code paths
		for _, tc := range []struct {
			name string
			id   any
			txs  []*types.Transaction
		}{
			{
				"executed_in_cache",
				blockMultiTxs.Hash(),
				multiTxs,
			},
			{
				"executed_in_cache_by_number",
				hexutil.Uint64(blockMultiTxs.Height()),
				multiTxs,
			},
			{
				"settled_in_db",
				blockSettled.Hash(),
				[]*types.Transaction{txSettled},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				requireBlockReceipts(t, tc.id, tc.txs)
			})
		}

		// Verify by-hash and by-number return equivalent receipts
		t.Run("hash_equals_number", func(t *testing.T) {
			var byHash, byNum []*types.Receipt
			require.NoError(t, sut.CallContext(ctx, &byHash, "eth_getBlockReceipts", blockMultiTxs.Hash()))
			require.NoError(t, sut.CallContext(ctx, &byNum, "eth_getBlockReceipts", hexutil.Uint64(blockMultiTxs.Height())))
			if diff := cmp.Diff(byHash, byNum, cmputils.Receipts()); diff != "" {
				t.Errorf("by-hash vs by-number diff (-hash +num):\n%s", diff)
			}
		})

		// Nil/empty returns
		sut.testRPC(ctx, t, []rpcTest{
			{
				method: "eth_getBlockReceipts",
				args:   []any{common.HexToHash("0xdeadbeef")},
				want:   ([]*types.Receipt)(nil),
			},
			{
				method: "eth_getBlockReceipts",
				args:   []any{genesis.Hash()},
				want:   []*types.Receipt{},
			},
		}...)
	})
}

func (sut *SUT) testGetByHash(ctx context.Context, t *testing.T, want *types.Block) {
	t.Helper()

	testRPCGetter(ctx, t, "eth_getBlockByHash", sut.BlockByHash, want.Hash(), want)
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getBlockByHash",
			args:   []any{want.Hash(), false},
			want:   want.Header(),
		},
		{
			method: "eth_getBlockTransactionCountByHash",
			args:   []any{want.Hash()},
			want:   hexutil.Uint(len(want.Transactions())),
		},
		{
			method: "eth_getUncleByBlockHashAndIndex",
			args:   []any{want.Hash(), hexutil.Uint(0)},
			want:   (map[string]any)(nil), // SAE never has uncles (no reorgs)
		},
		{
			method: "eth_getUncleCountByBlockHash",
			args:   []any{want.Hash()},
			want:   hexutil.Uint(0), // SAE never has uncles (no reorgs)
		},
	}...)

	for i, wantTx := range want.Transactions() {
		txIdx := hexutil.Uint(i) //nolint:gosec // definitely won't overflow
		marshaled, err := wantTx.MarshalBinary()
		require.NoErrorf(t, err, "%T.MarshalBinary()", wantTx)

		sut.testRPC(ctx, t, []rpcTest{
			{
				method: "eth_getTransactionByHash",
				args:   []any{wantTx.Hash()},
				want:   wantTx,
			},
			{
				method: "eth_getTransactionByBlockHashAndIndex",
				args:   []any{want.Hash(), txIdx},
				want:   wantTx,
			},
			{
				method: "eth_getRawTransactionByBlockHashAndIndex",
				args:   []any{want.Hash(), txIdx},
				want:   hexutil.Bytes(marshaled),
			},
			{
				method: "eth_getRawTransactionByHash",
				args:   []any{wantTx.Hash()},
				want:   hexutil.Bytes(marshaled),
			},
		}...)
	}

	outOfBoundsIndex := hexutil.Uint(len(want.Transactions()) + 1) //nolint:gosec // Known to not overflow
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getTransactionByBlockHashAndIndex",
			args:   []any{want.Hash(), outOfBoundsIndex},
			want:   (*types.Transaction)(nil),
		},
		{
			method: "eth_getRawTransactionByBlockHashAndIndex",
			args:   []any{want.Hash(), outOfBoundsIndex},
			want:   hexutil.Bytes(nil),
		},
	}...)
}

func (sut *SUT) testGetByUnknownHash(ctx context.Context, t *testing.T) {
	t.Helper()

	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getBlockByHash",
			args:   []any{common.Hash{}, true},
			want:   (*types.Block)(nil),
		},
		{
			method: "eth_getHeaderByHash",
			args:   []any{common.Hash{}},
			want:   (*types.Header)(nil),
		},
		{
			method: "eth_getBlockTransactionCountByHash",
			args:   []any{common.Hash{}},
			want:   (*hexutil.Uint)(nil),
		},
		{
			method: "eth_getTransactionByBlockHashAndIndex",
			args:   []any{common.Hash{}, hexutil.Uint(0)},
			want:   (*types.Transaction)(nil),
		},
		{
			method: "eth_getRawTransactionByBlockHashAndIndex",
			args:   []any{common.Hash{}, hexutil.Uint(0)},
			want:   hexutil.Bytes(nil),
		},
		{
			method: "eth_getTransactionByHash",
			args:   []any{common.Hash{}},
			want:   (*types.Transaction)(nil),
		},
		{
			method: "eth_getRawTransactionByHash",
			args:   []any{common.Hash{}},
			want:   hexutil.Bytes(nil),
		},
	}...)
}

// testGetByNumber accepts a block-number override to allow testing via named
// blocks, e.g. [rpc.LatestBlockNumber], not only via the specific number
// carried by the [types.Block].
func (sut *SUT) testGetByNumber(ctx context.Context, t *testing.T, want *types.Block, n rpc.BlockNumber) {
	t.Helper()
	testRPCGetter(ctx, t, "eth_getBlockByNumber", sut.BlockByNumber, big.NewInt(n.Int64()), want)

	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getBlockByNumber",
			args:   []any{n, false},
			want:   want.Header(),
		},
		{
			method: "eth_getBlockTransactionCountByNumber",
			args:   []any{n},
			want:   hexutil.Uint(len(want.Transactions())),
		},
		{
			method: "eth_getUncleByBlockNumberAndIndex",
			args:   []any{n, hexutil.Uint(0)},
			want:   (map[string]any)(nil), // SAE never has uncles (no reorgs)
		},
		{
			method: "eth_getUncleCountByBlockNumber",
			args:   []any{n},
			want:   hexutil.Uint(0), // SAE never has uncles (no reorgs)
		},
	}...)

	for i, wantTx := range want.Transactions() {
		txIdx := hexutil.Uint(i) //nolint:gosec // definitely won't overflow
		marshaled, err := wantTx.MarshalBinary()
		require.NoErrorf(t, err, "%T.MarshalBinary()", wantTx)

		sut.testRPC(ctx, t, []rpcTest{
			{
				method: "eth_getTransactionByBlockNumberAndIndex",
				args:   []any{n, txIdx},
				want:   wantTx,
			},
			{
				method: "eth_getRawTransactionByBlockNumberAndIndex",
				args:   []any{n, txIdx},
				want:   hexutil.Bytes(marshaled),
			},
		}...)
	}

	outOfBoundsIndex := hexutil.Uint(len(want.Transactions()) + 1) //nolint:gosec // Known to not overflow
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getTransactionByBlockNumberAndIndex",
			args:   []any{n, outOfBoundsIndex},
			want:   (*types.Transaction)(nil),
		},
		{
			method: "eth_getRawTransactionByBlockNumberAndIndex",
			args:   []any{n, outOfBoundsIndex},
			want:   hexutil.Bytes(nil),
		},
	}...)
}

func (sut *SUT) testGetByUnknownNumber(ctx context.Context, t *testing.T) {
	t.Helper()

	const n rpc.BlockNumber = math.MaxInt64
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_getBlockByNumber",
			args:   []any{n, true},
			want:   (*types.Block)(nil),
		},
		{
			method: "eth_getHeaderByNumber",
			args:   []any{n},
			want:   (*types.Header)(nil),
		},
		{
			method: "eth_getBlockTransactionCountByNumber",
			args:   []any{n},
			want:   (*hexutil.Uint)(nil),
		},
		{
			method: "eth_getTransactionByBlockNumberAndIndex",
			args:   []any{n, hexutil.Uint(0)},
			want:   (*types.Transaction)(nil),
		},
		{
			method: "eth_getRawTransactionByBlockNumberAndIndex",
			args:   []any{n, hexutil.Uint(0)},
			want:   hexutil.Bytes(nil),
		},
	}...)
}
