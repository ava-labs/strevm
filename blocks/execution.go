// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/trie"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/proxytime"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// SetInterimExecutionTime is expected to be called during execution of b's
// transactions, with the highest-known gas time. This MAY be at any resolution
// but MUST be monotonic.
func (b *Block) SetInterimExecutionTime(t *proxytime.Time[gas.Gas]) {
	b.interimExecutionTime.Store(t.Clone())
}

type executionResults struct {
	byGas  gastime.Time
	byWall time.Time // For metrics only; allowed to be incorrect.

	baseFee *big.Int
	// Receipts are deliberately not stored by the canoto representation as they
	// are already in the database. All methods that read the stored canoto
	// either accept a [types.Receipts] for comparison against the
	// `receiptRoot`, or don't care about receipts at all.
	receipts      types.Receipts
	receiptRoot   common.Hash
	stateRootPost common.Hash
}

// MarkExecuted marks the block as having been executed at the specified time(s)
// and with the specified results. It also sets the chain's head block to b.
//
// MarkExecuted guarantees that state is persisted to the database before
// in-memory indicators of execution are updated. [Block.Executed] returning
// true and [Block.WaitUntilExecuted] returning cleanly are both therefore
// indicative of a successful database write by MarkExecuted. The atomic pointer
// to the last-executed block is updated before [Block.WaitUntilExecuted]
// returns.
//
// This method MUST NOT be called more than once. The wall-clock [time.Time] is
// for metrics only.
func (b *Block) MarkExecuted(
	db ethdb.Database,
	byGas *gastime.Time,
	byWall time.Time,
	baseFee *big.Int,
	receipts types.Receipts,
	stateRootPost common.Hash,
	lastExecuted *atomic.Pointer[Block],
) error {
	e := &executionResults{
		byGas:         *byGas.Clone(),
		byWall:        byWall,
		baseFee:       new(big.Int).Set(baseFee),
		receipts:      slices.Clone(receipts),
		receiptRoot:   types.DeriveSha(receipts, trie.NewStackTrie(nil)),
		stateRootPost: stateRootPost,
	}

	// Disk
	batch := db.NewBatch()
	hash := b.Hash()
	rawdb.WriteHeadBlockHash(batch, hash)
	rawdb.WriteHeadHeaderHash(batch, hash)
	rawdb.WriteReceipts(batch, hash, b.NumberU64(), receipts)
	// TODO(arr4n) persist the [executionResults]
	if err := batch.Write(); err != nil {
		return err
	}

	// Memory and indicators
	return b.markExecuted(e, lastExecuted)
}

var errMarkBlockExecutedAgain = errors.New("block re-marked as executed")

func (b *Block) markExecuted(e *executionResults, lastExecuted *atomic.Pointer[Block]) error {
	if !b.execution.CompareAndSwap(nil, e) {
		// This is fatal because we corrupted the database's head block if we
		// got here by [Block.MarkExecuted] being called twice (an invalid use
		// of the API).
		b.log.Fatal("Block re-marked as executed")
		return fmt.Errorf("%w: height %d", errMarkBlockExecutedAgain, b.Height())
	}
	lastExecuted.Store(b)
	close(b.executed)
	return nil
}

// WaitUntilExecuted blocks until [Block.MarkExecuted] is called or the
// [context.Context] is cancelled.
func (b *Block) WaitUntilExecuted(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.executed:
		return nil
	}
}

// Executed reports whether [Block.MarkExecuted] has been called without
// resulting in an error.
func (b *Block) Executed() bool {
	return b.execution.Load() != nil
}

func executionArtefact[T any](b *Block, desc string, get func(*executionResults) T) T {
	e := b.execution.Load()
	if e == nil {
		b.log.Error("execution artefact requested before execution",
			zap.String("artefact", desc),
		)
		var zero T
		return zero
	}
	return get(e)
}

// ExecutedByGasTime returns a clone of the gas time passed to
// [Block.MarkExecuted] or nil if no such successful call has been made.
func (b *Block) ExecutedByGasTime() *gastime.Time {
	return executionArtefact(b, "execution (gas) time", func(e *executionResults) *gastime.Time {
		return e.byGas.Clone()
	})
}

// ExecutedByWallTime returns the wall time passed to [Block.MarkExecuted] or
// the zero time if no such successful call has been made.
func (b *Block) ExecutedByWallTime() time.Time {
	return executionArtefact(b, "execution (wall) time", func(e *executionResults) time.Time {
		return e.byWall
	})
}

// BaseFee returns the base gas price passed to [Block.MarkExecuted] or nil if
// no such successful call has been made.
func (b *Block) BaseFee() *big.Int {
	return executionArtefact(b, "receipts", func(e *executionResults) *big.Int {
		return new(big.Int).Set(e.baseFee)
	})
}

// Receipts returns the receipts passed to [Block.MarkExecuted] or nil if no
// such successful call has been made.
func (b *Block) Receipts() types.Receipts {
	return executionArtefact(b, "receipts", func(e *executionResults) types.Receipts {
		return slices.Clone(e.receipts)
	})
}

// PostExecutionStateRoot returns the state root passed to [Block.MarkExecuted]
// or the zero hash if no such successful call has been made.
func (b *Block) PostExecutionStateRoot() common.Hash {
	return executionArtefact(b, "state root", func(e *executionResults) common.Hash {
		return e.stateRootPost
	})
}
