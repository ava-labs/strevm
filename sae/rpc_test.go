// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/cmputils"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest"
)

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
	ctx, sut := newSUT(t, 1)
	testRPCMethod(ctx, t, sut, "web3_clientVersion", version.GetVersions().String())
	var (
		preImage hexutil.Bytes = []byte("test")
		want                   = hexutil.Bytes(crypto.Keccak256(preImage))
	)
	testRPCMethod(ctx, t, sut, "web3_sha3", want, preImage)
}

func TestNetNamespace(t *testing.T) {
	testRPCMethodsWithPeers := func(sut *SUT, wantPeerCount hexutil.Uint) {
		t.Helper()

		ctx := sut.context(t)
		testRPCMethod(ctx, t, sut, "net_listening", true)
		testRPCMethod(ctx, t, sut, "net_peerCount", wantPeerCount)
		testRPCMethod(ctx, t, sut, "net_version", fmt.Sprintf("%d", saetest.ChainConfig().ChainID.Uint64()))
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

func testRPCMethod[T any](ctx context.Context, t *testing.T, sut *SUT, method string, want T, args ...any) {
	t.Helper()
	t.Run(method, func(t *testing.T) {
		var got T
		t.Logf("%T.CallContext(ctx, %T, %q, %v...)", sut.rpcClient, got, method, args)
		require.NoError(t, sut.CallContext(ctx, &got, method, args...))

		opts := cmp.Options{
			cmpopts.IgnoreFields(types.Header{}, "extra"),
			cmputils.BigInts(),
		}

		if diff := cmp.Diff(want, got, opts); diff != "" {
			t.Errorf("Diff (-want +got):\n%s", diff)
		}
	})
}

func TestBlockIdentification(t *testing.T) {
	opt, setTime := stubbedTime()
	ctx, sut := newSUT(t, 1, opt)

	now := time.Unix(100, 0)
	setTime(now)

	// The named block "pending" is the last to be enqueued but yet to be
	// executed. Although unlikely to be useful in practice, it still needs to
	// be tested.
	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	unblockPrecompile := make(chan struct{})
	t.Cleanup(func() { close(unblockPrecompile) })

	libevmHooks := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			blockingPrecompile: vm.NewStatefulPrecompile(func(vm.PrecompileEnvironment, []byte) ([]byte, error) {
				<-unblockPrecompile
				return nil, nil
			}),
		},
	}
	libevmHooks.Register(t)

	genesis := sut.lastAcceptedBlock(t)
	// Anything older than the last-settled block is cleared from [VM.blocks]
	// so is guaranteed to be read from the database.
	dbOnly := sut.runConsensusLoop(t, genesis)
	settled := sut.runConsensusLoop(t, dbOnly)
	require.NoErrorf(t, settled.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted() for settlement", settled)

	setTime(now.Add(params.Tau))
	executed := sut.runConsensusLoop(t, settled)
	require.Truef(t, settled.Settled(), "%T.Settled()", settled)
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted() for last-executed block", executed)

	tx := sut.wallet.SetNonceAndSign(t, 0, &types.LegacyTx{
		To:       &blockingPrecompile,
		GasPrice: big.NewInt(1),
		Gas:      1e6, // arbitrary but sufficiently high
	})
	sut.mustSendTx(t, tx)
	require.EventuallyWithTf(t, func(c *assert.CollectT) {
		sut.syncMempool(t)
		p := sut.rawVM.mempool.Pool
		require.Truef(c, p.Has(tx.Hash()), "%T.Has(%v)", p, tx.Hash())
	}, time.Second, 10*time.Millisecond, "New transaction %v in mempool", tx.Hash())
	pending := sut.runConsensusLoop(t, executed)
	require.Len(t, pending.Transactions(), 1, "txs in pending block")

	// [ethclient.Client.BlockByNumber] isn't compatible with pending blocks as
	// the geth RPC server strips fields that the client then expects to find.
	testRPCMethod(ctx, t, sut, "eth_getBlockByNumber", pending.Header(), rpc.PendingBlockNumber, true)

	byHash := cmp.Comparer(func(a, b *types.Block) bool {
		return a.Hash() == b.Hash()
	})

	t.Run("BlockByNumber", func(t *testing.T) {
		tests := []struct {
			name string
			num  rpc.BlockNumber
			want *types.Block
		}{
			{
				name: "genesis",
				num:  0,
				want: genesis.EthBlock(),
			},
			{
				name: "latest",
				num:  rpc.LatestBlockNumber,
				want: executed.EthBlock(),
			},
			{
				name: "safe",
				num:  rpc.SafeBlockNumber,
				want: settled.EthBlock(),
			},
			{
				name: "finalized",
				num:  rpc.FinalizedBlockNumber,
				want: settled.EthBlock(),
			},
			{
				name: "from_db",
				num:  rpc.BlockNumber(dbOnly.Number().Int64()),
				want: dbOnly.EthBlock(),
			},
		}

		cl := sut.Client

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := cl.BlockByNumber(ctx, big.NewInt(tt.num.Int64()))
				require.NoErrorf(t, err, "%T.BlockByNumber(%v)", cl, tt.num)
				if diff := cmp.Diff(tt.want, got, byHash); diff != "" {
					t.Errorf("BlockByNumber(%v) diff (-want +got):\n%s", tt.num, diff)
				}
			})
		}

	})

	t.Run("BlockByHash", func(t *testing.T) {

	})
}
