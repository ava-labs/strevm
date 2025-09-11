// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/saetest"
)

// markExecutedForTests calls [Block.MarkExecuted] with zero-value
// post-execution artefacts (other than the gas time).
func (b *Block) markExecutedForTests(tb testing.TB, db ethdb.Database, tm *gastime.Time) {
	tb.Helper()
	require.NoError(tb, b.MarkExecuted(db, tm, time.Time{}, nil, common.Hash{}), "MarkExecuted()")
}

func TestMarkExecuted(t *testing.T) {
	txs := make(types.Transactions, 10)
	for i := range txs {
		txs[i] = types.NewTx(&types.LegacyTx{
			Nonce: uint64(i), //nolint:gosec // Won't overflow
		})
	}

	ethB := types.NewBlock(
		&types.Header{
			Number: big.NewInt(1),
			Time:   42,
		},
		txs,
		nil, nil, // uncles, receipts
		saetest.TrieHasher(),
	)
	db := rawdb.NewMemoryDatabase()
	rawdb.WriteBlock(db, ethB)

	settles := newBlock(t, newEthBlock(0, 0, nil), nil, nil)
	settles.markExecutedForTests(t, db, gastime.New(0, 1, 0))
	b := newBlock(t, ethB, nil, settles)

	t.Run("before_MarkExecuted", func(t *testing.T) {
		require.False(t, b.Executed(), "Executed()")
		require.NoError(t, b.CheckInvariants(NotExecuted), "CheckInvariants(NotExecuted)")

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, context.DeadlineExceeded, b.WaitUntilExecuted(ctx), "WaitUntilExecuted()")

		var rec saetest.LogRecorder
		b.log = &rec
		assertNumErrorLogs := func(t *testing.T, want int) {
			t.Helper()
			assert.Len(t, rec.At(logging.Error), want, "Number of ERROR logs")
		}

		tests := []struct {
			method string
			call   func() any
		}{
			{"ExecutedByGasTime()", func() any { return b.ExecutedByGasTime() }},
			{"Receipts()", func() any { return b.Receipts() }},
			{"PostExecutionStateRoot()", func() any { return b.PostExecutionStateRoot() }},
		}
		for i, tt := range tests {
			assert.Zero(t, tt.call(), tt.method)
			assertNumErrorLogs(t, i+1)
		}
	})

	gasTime := gastime.New(42, 1e6, 42)
	wallTime := time.Unix(42, 100)
	stateRoot := common.Hash{'s', 't', 'a', 't', 'e'}
	var receipts types.Receipts
	for _, tx := range txs {
		receipts = append(receipts, &types.Receipt{
			TxHash: tx.Hash(),
		})
	}
	require.NoError(t, b.MarkExecuted(db, gasTime, wallTime, receipts, stateRoot), "MarkExecuted()")

	assertPostExecutionVals := func(t *testing.T, b *Block) {
		t.Helper()
		require.True(t, b.Executed(), "Executed()")
		assert.NoError(t, b.CheckInvariants(Executed), "CheckInvariants(Executed)")

		require.NoError(t, b.WaitUntilExecuted(context.Background()), "WaitUntilExecuted()")

		assert.Zero(t, b.ExecutedByGasTime().Compare(gasTime.Time), "ExecutedByGasTime().Compare([original input])")
		assert.Empty(t, cmp.Diff(receipts, b.Receipts(), saetest.CmpByMerkleRoots[types.Receipts]()), "Receipts()")

		assert.Equal(t, stateRoot, b.PostExecutionStateRoot(), "PostExecutionStateRoot()") // i.e. this block
		// Although not directly relevant to MarkExecuted, demonstrate that the
		// two notion's of a state root are in fact different.
		assert.Equal(t, settles.Block.Root(), b.SettledStateRoot(), "SettledStateRoot()") // i.e. the block this block settles
		assert.NotEqual(t, b.SettledStateRoot(), b.PostExecutionStateRoot(), "PostExecutionStateRoot() != SettledStateRoot()")

		t.Run("MarkExecuted_again", func(t *testing.T) {
			var rec saetest.LogRecorder
			b.log = &rec
			assert.ErrorIs(t, b.MarkExecuted(db, gasTime, wallTime, receipts, stateRoot), errMarkBlockExecutedAgain)
			// The database's head block might have been corrupted so this MUST
			// be a fatal action.
			assert.Len(t, rec.At(logging.Fatal), 1, "FATAL logs")
		})
	}
	t.Run("after_MarkExecuted", func(t *testing.T) {
		assertPostExecutionVals(t, b)
	})

	t.Run("database", func(t *testing.T) {
		t.Run("RestorePostExecutionStateAndReceipts", func(t *testing.T) {
			clone := newBlock(t, b.Block, nil, settles)
			err := clone.RestorePostExecutionStateAndReceipts(
				db,
				params.TestChainConfig, // arbitrary
			)
			require.NoError(t, err)
			assertPostExecutionVals(t, clone)
		})

		t.Run("StateRootPostExecution", func(t *testing.T) {
			got, err := StateRootPostExecution(db, b.NumberU64())
			require.NoError(t, err)
			assert.Equal(t, stateRoot, got)
		})

		t.Run("head_block", func(t *testing.T) {
			for fn, got := range map[string]interface{ Hash() common.Hash }{
				"ReadHeadBlockHash":  selfAsHasher(rawdb.ReadHeadBlockHash(db)),
				"ReadHeadHeaderHash": selfAsHasher(rawdb.ReadHeadHeaderHash(db)),
				"ReadHeadBlock":      rawdb.ReadHeadBlock(db),
				"ReadHeadHeader":     rawdb.ReadHeadHeader(db),
			} {
				t.Run(fmt.Sprintf("rawdb.%s", fn), func(t *testing.T) {
					require.NotNil(t, got)
					assert.Equalf(t, b.Hash(), got.Hash(), "rawdb.%s()", fn)
				})
			}
		})
	})
}

// selfAsHasher adds a Hash() method to a common.Hash, returning itself.
type selfAsHasher common.Hash

func (h selfAsHasher) Hash() common.Hash { return common.Hash(h) }
