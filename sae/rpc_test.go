// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/arr4n/shed/testerr"
	"github.com/ava-labs/avalanchego/version"
	ethereum "github.com/ava-labs/libevm"
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
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/cmputils"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saetest/escrow"
)

var zeroAddr common.Address

type rpcTest struct {
	method  string
	args    []any
	want    any // untyped nil means no return value.
	wantErr testerr.Want
}

func (s *SUT) testRPC(ctx context.Context, t *testing.T, tcs ...rpcTest) {
	t.Helper()
	opts := []cmp.Option{
		cmputils.NilSlicesAreEmpty[hexutil.Bytes](),
		cmputils.Headers(),
		cmputils.HexutilBigs(),
		cmputils.TransactionsByHash(),
		cmputils.Receipts(),
	}

	for _, tc := range tcs {
		t.Run(tc.method, func(t *testing.T) {
			if tc.want == nil { // Reminder: only applies to untyped nil
				tc.want = struct{ json.RawMessage }{} // struct avoids nil vs empty
			}

			got := reflect.New(reflect.TypeOf(tc.want))
			t.Logf("%T.CallContext(ctx, %T, %q, %v...)", s.rpcClient, &tc.want, tc.method, tc.args)
			err := s.CallContext(ctx, got.Interface(), tc.method, tc.args...)
			if diff := testerr.Diff(err, tc.wantErr); diff != "" {
				t.Fatalf("CallContext(...) %s", diff)
			}
			if diff := cmp.Diff(tc.want, got.Elem().Interface(), opts...); diff != "" {
				t.Errorf("Unmarshalled %T diff (-want +got):\n%s", got.Elem().Interface(), diff)
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

	var (
		newTxs   = make(chan common.Hash, 1)
		newHeads = make(chan *types.Header, 1)
		newLogs  = make(chan types.Log, 1)
	)
	{
		sub, err := sut.rpcClient.EthSubscribe(ctx, newTxs, "newPendingTransactions")
		require.NoError(t, err, "EthSubscribe(newPendingTransactions)")
		t.Cleanup(sub.Unsubscribe)
	}
	{
		sub, err := sut.SubscribeNewHead(ctx, newHeads)
		require.NoError(t, err, "SubscribeNewHead()")
		t.Cleanup(sub.Unsubscribe)
	}
	{
		sub, err := sut.SubscribeFilterLogs(ctx, ethereum.FilterQuery{}, newLogs)
		require.NoError(t, err, "SubscribeFilterLogs()")
		t.Cleanup(sub.Unsubscribe)
	}
	{
		pendingLogs := make(chan types.Log, 1)
		pendingBlock := big.NewInt(int64(rpc.PendingBlockNumber))
		sub, err := sut.SubscribeFilterLogs(
			ctx,
			ethereum.FilterQuery{
				FromBlock: pendingBlock,
				ToBlock:   pendingBlock,
			},
			pendingLogs,
		)
		require.NoError(t, err, "SubscribeFilterLogs(pending)")
		t.Cleanup(sub.Unsubscribe)
		defer func() {
			t.Helper()

			select {
			case l := <-pendingLogs:
				t.Fatalf("unexpected pending log %+v", l)
			default:
			}
		}()
	}

	mustSendTx := func(tx *types.Transaction) {
		t.Helper()

		sut.mustSendTx(t, tx)
		require.Equal(t, tx.Hash(), <-newTxs, "tx hash from newPendingTransactions subscription")
	}

	runConsensusLoop := func(wantLogs ...types.Log) {
		t.Helper()

		b := sut.runConsensusLoop(t, sut.lastAcceptedBlock(t))
		require.Equal(t, b.Hash(), (<-newHeads).Hash(), "header hash from newHeads subscription")

		for _, want := range wantLogs {
			want.BlockNumber = b.NumberU64()
			want.BlockHash = b.Hash()
			require.Equal(t, want, <-newLogs, "log from subscription")
		}
	}

	const senderIndex = 0
	deployTx := sut.wallet.SetNonceAndSign(t, senderIndex, &types.LegacyTx{
		Data:     escrow.CreationCode(),
		GasPrice: big.NewInt(1),
		Gas:      3e6,
	})
	mustSendTx(deployTx)
	runConsensusLoop()

	sender := sut.wallet.Addresses()[senderIndex]
	contractAddr := crypto.CreateAddress(sender, deployTx.Nonce())
	amount := uint256.NewInt(100)
	depositTx := sut.wallet.SetNonceAndSign(t, senderIndex, &types.LegacyTx{
		To:       &contractAddr,
		Value:    amount.ToBig(),
		Data:     escrow.CallDataToDeposit(sender),
		GasPrice: big.NewInt(1),
		Gas:      1e6,
	})
	mustSendTx(depositTx)

	wantLog := escrow.DepositEvent(sender, amount)
	wantLog.Address = contractAddr
	wantLog.TxHash = depositTx.Hash()
	runConsensusLoop(*wantLog)
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

// registerBlockingPrecompile registers `addr` as a libevm precompile such that
// any transactions sent to the precompile will block until the returned
// function is called. It is safe to call the unblocker multiple times, which
// will also be done during cleanup.
func registerBlockingPrecompile(tb testing.TB, addr common.Address) func() {
	tb.Helper()
	unblock := make(chan struct{})
	libevmHooks := &libevmhookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			addr: vm.NewStatefulPrecompile(func(vm.PrecompileEnvironment, []byte) ([]byte, error) {
				<-unblock
				return nil, nil
			}),
		},
	}
	libevmHooks.Register(tb)

	fn := sync.OnceFunc(func() { close(unblock) })
	tb.Cleanup(fn)
	return fn
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
	registerBlockingPrecompile(t, blockingPrecompile)

	createTx := func(t *testing.T, to common.Address) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &to,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}

	genesis := sut.lastAcceptedBlock(t)

	// Once a block is settled, its ancestors are only accessible from the
	// database.
	onDisk := sut.createAndAcceptBlock(t, createTx(t, zeroAddr))

	settled := sut.createAndAcceptBlock(t, createTx(t, zeroAddr))
	vmTime.advanceToSettle(ctx, t, settled)

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

func TestGetLogs(t *testing.T) {
	// We shorten section size to reduce number of required blocks in the test.
	const bloomSectionSize = 8
	timeOpt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))

	ctx, sut := newSUT(t, 1, timeOpt, withBloomSectionSize(bloomSectionSize))
	genesis := sut.lastAcceptedBlock(t)

	emitter := common.Address{'l', 'o', 'g'}
	rng := crypto.NewKeccakState()
	stub := &libevmhookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			emitter: vm.NewStatefulPrecompile(func(env vm.PrecompileEnvironment, _ []byte) ([]byte, error) {
				data := make([]byte, 8)
				rng.Read(data) //nolint:gosec,errcheck // Never returns an error; signature only to implement io.Reader
				env.StateDB().AddLog(&types.Log{
					Address: env.Addresses().EVMSemantic.Self,
					Data:    data, // Guarantee uniqueness as this is the data under test
				})
				return nil, nil
			}),
		},
	}
	stub.Register(t)

	txWithLog := func(t *testing.T) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       &emitter,
			GasPrice: big.NewInt(1),
			Gas:      1e6,
		})
	}
	txWithoutLog := func(t *testing.T) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       &common.Address{},
			GasPrice: big.NewInt(1),
			Gas:      params.TxGas,
		})
	}

	// Create enough blocks to ensure some are indexed.
	// Since a block after this will be settled, these are all settled as well,
	// and therefore moved to disk.
	indexed := make([]*blocks.Block, bloomSectionSize)
	for i := range indexed {
		indexed[i] = sut.createAndAcceptBlock(t, txWithLog(t))
	}

	settled := sut.createAndAcceptBlock(t, txWithLog(t))
	vmTime.advanceToSettle(ctx, t, settled)

	noLogs := sut.createAndAcceptBlock(t, txWithoutLog(t))

	executed := sut.createAndAcceptBlock(t, txWithLog(t))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	// Although the FiltersAPI will work without any blocks indexed, such a
	// scenario would not test the functionality of the bloom indexer.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		be := sut.rawVM.APIBackend()
		_, got := be.BloomStatus()
		require.Equal(c, uint64(1), got, "%T.BloomStatus() sections", be)
	}, 5*time.Second, 100*time.Millisecond, "bloom indexer never finished")

	logsFrom := func(bs ...*blocks.Block) []types.Log {
		var logs []types.Log
		for _, b := range bs {
			for _, r := range b.Receipts() {
				for _, l := range r.Logs {
					logs = append(logs, *l)
				}
			}
		}
		return logs
	}

	ptr := func(h common.Hash) *common.Hash { return &h }

	tests := []struct {
		name            string
		query           ethereum.FilterQuery
		wantLogs        []types.Log
		wantErrContains string
	}{
		{
			name: "genesis",
			query: ethereum.FilterQuery{
				BlockHash: ptr(genesis.Hash()),
			},
		},
		{
			name: "no_logs",
			query: ethereum.FilterQuery{
				BlockHash: ptr(noLogs.Hash()),
			},
		},
		{
			name: "nonexistent_block",
			query: ethereum.FilterQuery{
				BlockHash: &common.Hash{0xde, 0xad},
			},
			wantErrContains: "unknown block",
		},
		{
			name: "on_disk",
			query: ethereum.FilterQuery{
				BlockHash: ptr(indexed[0].Hash()),
			},
			wantLogs: logsFrom(indexed[0]),
		},
		{
			name: "in_memory",
			query: ethereum.FilterQuery{
				BlockHash: ptr(executed.Hash()),
			},
			wantLogs: logsFrom(executed),
		},
		{
			name: "unindexed",
			query: ethereum.FilterQuery{
				FromBlock: settled.Number(),
				ToBlock:   executed.Number(),
				Addresses: []common.Address{emitter},
			},
			wantLogs: logsFrom(settled, executed),
		},
		{
			name: "unknown_contract",
			query: ethereum.FilterQuery{
				FromBlock: settled.Number(),
				ToBlock:   executed.Number(),
				Addresses: []common.Address{{0xf0, 0x00}},
			},
		},
		{
			name: "indexed",
			query: ethereum.FilterQuery{
				FromBlock: indexed[0].Number(),
				ToBlock:   indexed[len(indexed)-1].Number(),
				Addresses: []common.Address{emitter},
			},
			wantLogs: logsFrom(indexed...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("%T: %[1]v", tt.query)
			got, err := sut.FilterLogs(ctx, tt.query)
			if tt.wantErrContains != "" {
				require.ErrorContains(t, err, tt.wantErrContains, "eth_getLogs(...)")
				return
			}
			require.NoErrorf(t, err, "eth_getLogs(...)")
			if diff := cmp.Diff(tt.wantLogs, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("eth_getLogs(...) mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEthPendingTransactions(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	tx := sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
		To:        &zeroAddr,
		Gas:       params.TxGas,
		GasFeeCap: big.NewInt(1),
		Value:     big.NewInt(100),
	})
	sut.mustSendTx(t, tx)
	sut.requireInMempool(t, tx.Hash())

	// eth_pendingTransactions filters results to only transactions from
	// accounts configured in the AccountManager, which is always empty.
	sut.testRPC(ctx, t, rpcTest{
		method: "eth_pendingTransactions",
		want:   []*ethapi.RPCTransaction{},
	})
}

func TestGetReceipts(t *testing.T) {
	timeOpt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, timeOpt)

	// Blocking precompile creates accepted-but-not-executed blocks
	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	registerBlockingPrecompile(t, blockingPrecompile)

	var (
		txs  []*types.Transaction
		want []*types.Receipt
	)
	for range 6 {
		tx := sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
			To:       &zeroAddr,
			Gas:      params.TxGas,
			GasPrice: big.NewInt(1),
		})
		txs = append(txs, tx)
		want = append(want, &types.Receipt{
			TxHash:            tx.Hash(),
			Status:            types.ReceiptStatusSuccessful,
			GasUsed:           params.TxGas,
			EffectiveGasPrice: big.NewInt(1),
			Logs:              []*types.Log{},
		})
	}

	slice := func(t *testing.T, from, to int) (*blocks.Block, []*types.Receipt) {
		t.Helper()
		b := sut.createAndAcceptBlock(t, txs[from:to]...)
		rs := want[from:to]

		var totalGas uint64
		for i, r := range rs {
			totalGas += r.GasUsed
			r.CumulativeGasUsed = totalGas
			r.BlockHash = b.Hash()
			r.BlockNumber = b.Number()
			r.TransactionIndex = uint(i) //nolint:gosec // Known non-negative
		}
		return b, rs
	}

	genesis := sut.lastAcceptedBlock(t)

	onDisk, wantOnDisk := slice(t, 0, 2)
	settled, wantSettled := slice(t, 2, 4)
	vmTime.advanceToSettle(ctx, t, settled)
	unsettled, wantUnsettled := slice(t, 4, 6)
	sut.waitUntilExecuted(t, unsettled)

	// Pending block: accepted but not yet executed (blocking precompile).
	pendingTx := sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
		To:       &blockingPrecompile,
		Gas:      params.TxGas,
		GasPrice: big.NewInt(1),
	})
	pending := sut.createAndAcceptBlock(t, pendingTx)

	var tests []rpcTest
	for _, tc := range []struct {
		id   rpc.BlockNumberOrHash
		want []*types.Receipt
	}{
		{
			id:   rpc.BlockNumberOrHashWithHash(onDisk.Hash(), true),
			want: wantOnDisk,
		},
		{
			id:   rpc.BlockNumberOrHashWithHash(settled.Hash(), true),
			want: wantSettled,
		},
		{
			id:   rpc.BlockNumberOrHashWithHash(unsettled.Hash(), true),
			want: wantUnsettled,
		},
		{
			id:   rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber),
			want: wantUnsettled,
		},
	} {
		tests = append(tests, rpcTest{
			method: "eth_getBlockReceipts",
			args:   []any{tc.id.String()},
			want:   tc.want,
		})
	}

	for i, tx := range txs {
		tests = append(tests, rpcTest{
			method: "eth_getTransactionReceipt",
			args:   []any{tx.Hash()},
			want:   want[i],
		})
	}

	// Acceptance writes blocks to the DB but not receipts, so pending
	// block receipts error while pending tx receipts return nil.
	tests = append(tests, []rpcTest{
		{
			method: "eth_getTransactionReceipt",
			args:   []any{pendingTx.Hash()},
			want:   (*types.Receipt)(nil),
		},
		{
			method: "eth_getTransactionReceipt",
			args:   []any{common.Hash{}},
			want:   (*types.Receipt)(nil),
		},
		{
			method: "eth_getBlockReceipts",
			args:   []any{common.Hash{}},
			want:   ([]*types.Receipt)(nil),
		},
		{
			method: "eth_getBlockReceipts",
			args:   []any{genesis.Hash()},
			want:   []*types.Receipt{},
		},
		{
			method:  "eth_getBlockReceipts",
			args:    []any{pending.Hash()},
			wantErr: testerr.Contains("receipts length mismatch"),
		},
		{
			method:  "eth_getBlockReceipts",
			args:    []any{hexutil.Uint64(pending.Height())},
			wantErr: testerr.Contains("receipts length mismatch"),
		},
	}...)

	sut.testRPC(ctx, t, tests...)

	t.Run("requireCanonical_on_in_memory_hit", func(t *testing.T) {
		height := unsettled.Height()
		originalCanonical := rawdb.ReadCanonicalHash(sut.db, height)
		require.Equal(t, unsettled.Hash(), originalCanonical)

		rawdb.WriteCanonicalHash(sut.db, common.HexToHash("0x6869207374657068656E"), height)
		t.Cleanup(func() {
			rawdb.WriteCanonicalHash(sut.db, originalCanonical, height)
		})

		sut.testRPC(ctx, t, rpcTest{
			method: "eth_getBlockReceipts",
			args: []any{
				map[string]any{
					"blockHash":        unsettled.Hash(),
					"requireCanonical": true,
				},
			},
			want: ([]*types.Receipt)(nil),
		})
	})

	t.Run("malformed_xdb", func(t *testing.T) {
		// onDisk was naturally evicted from the in-memory blocks map
		// when its descendant settled, so this exercises the DB path.
		require.NoErrorf(t, onDisk.WaitUntilSettled(ctx), "%T.WaitUntilSettled()", onDisk)
		require.NoError(t, sut.rawVM.xdb.Put(onDisk.Height(), []byte{0x01}))

		sut.testRPC(ctx, t, rpcTest{
			method:  "eth_getTransactionReceipt",
			args:    []any{txs[0].Hash()},
			wantErr: testerr.Contains("restoring execution artefacts"),
		})
	})
}

// SAE doesn't really support APIs that require a key on the node, as there is
// no way to add keys. But, we want to ensure the methods error gracefully.
func TestEthSigningAPIs(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	wantErr := testerr.Contains("unknown account")
	txFields := map[string]any{
		"from":     zeroAddr,
		"to":       zeroAddr,
		"gas":      hexutil.Uint64(params.TxGas),
		"gasPrice": hexutil.Big(*big.NewInt(1)),
		"value":    hexutil.Big(*big.NewInt(100)),
		"nonce":    hexutil.Uint64(0),
	}
	sut.testRPC(ctx, t, []rpcTest{
		{
			method: "eth_sign",
			args: []any{
				zeroAddr,
				hexutil.Bytes("test message"),
			},
			wantErr: wantErr,
		},
		{
			method: "eth_signTransaction",
			args: []any{
				txFields,
			},
			wantErr: wantErr,
		},
		{
			method: "eth_sendTransaction",
			args: []any{
				txFields,
			},
			wantErr: wantErr,
		},
	}...)
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

// withDebugAPI returns a sutOption that enables the debug API.
func withDebugAPI() sutOption {
	return options.Func[sutConfig](func(c *sutConfig) {
		c.vmConfig.RPCConfig.EnableProfiling = true
	})
}

func TestDebugNamespace(t *testing.T) {
	ctx, sut := newSUT(t, 0, withDebugAPI())

	// The debug namespace is handled entirely by upstream code that doesn't
	// depend in any way on SAE. We therefore only need an integration test, not
	// to exercise every method because such unit testing is the responsibility
	// of the source.
	//
	// Reference: https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug

	const firstArg = 100
	beforeTest := debug.SetGCPercent(firstArg)
	defer debug.SetGCPercent(beforeTest)

	const m = "debug_setGCPercent"
	sut.testRPC(ctx, t, []rpcTest{
		// Invariant: each call returns the input argument of the last.
		{
			method: m,
			args:   []any{42},
			want:   firstArg,
		},
		{
			method: m,
			args:   []any{0},
			want:   42,
		},
	}...)
}
