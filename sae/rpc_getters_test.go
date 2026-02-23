// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	saeparams "github.com/ava-labs/strevm/params"
)

func TestEthGetters(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, opt)

	// The named block "pending" is the last to be enqueued but yet to be
	// executed. Although unlikely to be useful in practice, it still needs to
	// be tested.
	blockingPrecompile := common.Address{'b', 'l', 'o', 'c', 'k'}
	registerBlockingPrecompile(t, blockingPrecompile)

	genesis := sut.lastAcceptedBlock(t)

	createTx := func(t *testing.T, to common.Address) *types.Transaction {
		t.Helper()
		return sut.wallet.SetNonceAndSign(t, 0, &types.DynamicFeeTx{
			To:        &to,
			Gas:       params.TxGas,
			GasFeeCap: big.NewInt(1),
		})
	}

	// TODO(arr4n) abstract the construction of blocks at all stages as it's
	// copy-pasta'd elsewhere.

	var zero common.Address
	// Once a block is settled, its ancestors are only accessible from the
	// database.
	onDisk := sut.runConsensusLoop(t, createTx(t, zero))

	settled := sut.runConsensusLoop(t, createTx(t, zero))
	vmTime.advanceToSettle(ctx, t, settled)

	executed := sut.runConsensusLoop(t, createTx(t, zero))
	require.NoErrorf(t, executed.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", executed)

	pending := sut.runConsensusLoop(t, createTx(t, blockingPrecompile))

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

func TestGetByUnknownHash(t *testing.T) {
	ctx, sut := newSUT(t, 0)

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

func TestGetByUnknownNumber(t *testing.T) {
	ctx, sut := newSUT(t, 0)

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
