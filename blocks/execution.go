// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"context"
	"encoding/binary"
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
	"github.com/holiman/uint256"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/proxytime"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

// SetInterimExecutionTime is expected to be called during execution of b's
// transactions, with the highest-known gas time. This MAY be at any resolution
// but MUST be monotonic.
func (b *Block) SetInterimExecutionTime(t *proxytime.Time[gas.Gas]) {
	b.interimExecutionTime.Store(t.Clone())
}

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

type executionResults struct {
	byGas  gastime.Time `canoto:"value,1"`
	byWall time.Time    // For metrics only; allowed to be incorrect.

	baseFee [4]uint64 `canoto:"fixed repeated uint,2"` // Interpreted as [uint256.Int]
	// Receipts are deliberately not stored by the canoto representation as they
	// are already in the database. All methods that read the stored canoto
	// either accept a [types.Receipts] for comparison against the
	// `receiptRoot`, or don't care about receipts at all.
	receipts      types.Receipts
	receiptRoot   common.Hash `canoto:"fixed bytes,3"`
	stateRootPost common.Hash `canoto:"fixed bytes,4"`

	canotoData canotoData_executionResults
}

func (e *executionResults) setBaseFee(bf *big.Int) error {
	if bf == nil { // genesis blocks
		return nil
	}
	if overflow := (*uint256.Int)(&e.baseFee).SetFromBig(bf); overflow {
		return fmt.Errorf("base fee %v overflows 256 bits", bf)
	}
	return nil
}

func (e *executionResults) persist(kvw ethdb.KeyValueWriter, blockNum uint64) error {
	return kvw.Put(executionResultsKey(blockNum), e.MarshalCanoto())
}

func executionResultsKey(blockNum uint64) []byte {
	const prefix = params.RawDBPrefix + "exec-"
	n := len(prefix)
	key := make([]byte, n, n+8)
	copy(key[:n], prefix)
	return binary.BigEndian.AppendUint64(key, blockNum)
}

// ReadBaseFeeFromExecutionResults reads the execution-time base fee stored
// in the database for the given block number.
func ReadBaseFeeFromExecutionResults(db ethdb.Database, blockNum uint64) (*uint256.Int, error) {
	buf, err := db.Get(executionResultsKey(blockNum))
	if err != nil {
		return nil, err
	}

	e := new(executionResults)
	if err := e.UnmarshalCanoto(buf); err != nil {
		return nil, err
	}

	baseFee := uint256.Int(e.baseFee)
	return &baseFee, nil
}

// MarkExecuted marks the block as having been executed at the specified time(s)
// and with the specified results. It also sets the chain's head block to b. The
// [gastime.Time] MUST have already been scaled to the target applicable after
// the block, as defined by the relevant [hook.Points].
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
	if it := b.interimExecutionTime.Load(); it != nil && byGas.Compare(it) < 0 {
		// The final execution time is scaled to the new gas target but interim
		// times are not, which can result in rounding errors. Scaling always
		// rounds up, to maintain a monotonic clock, but we confirm for safety.
		// The logger used in tests will also convert this to a failure.
		b.log.Error("Final execution gas time before last interim time",
			zap.Stringer("interim_time", it),
			zap.Stringer("final_time", byGas.Time),
		)
	}

	e := &executionResults{
		byGas:         *byGas.Clone(),
		byWall:        byWall,
		receipts:      slices.Clone(receipts),
		receiptRoot:   types.DeriveSha(receipts, trie.NewStackTrie(nil)),
		stateRootPost: stateRootPost,
	}
	if err := e.setBaseFee(baseFee); err != nil {
		return err
	}

	// Disk
	batch := db.NewBatch()
	rawdb.WriteReceipts(batch, b.Hash(), b.NumberU64(), receipts)
	b.SetAsHeadBlock(batch)
	if err := e.persist(batch, b.NumberU64()); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}

	// Memory and indicators
	return b.markExecuted(e, lastExecuted)
}

func (b *Block) SetAsHeadBlock(kvw ethdb.KeyValueWriter) {
	h := b.Hash()
	rawdb.WriteHeadBlockHash(kvw, h)
	rawdb.WriteHeadHeaderHash(kvw, h)
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
	if lastExecuted != nil {
		lastExecuted.Store(b)
	}
	close(b.executed)
	return nil
}

func (b *Block) ReloadExecutionResults(db ethdb.Database) error {
	buf, err := db.Get(executionResultsKey(b.NumberU64()))
	if err != nil {
		return err
	}
	e := new(executionResults)
	if err := e.UnmarshalCanoto(buf); err != nil {
		return err
	}
	e.receipts = rawdb.ReadRawReceipts(db, b.Hash(), b.NumberU64())
	return b.markExecuted(e, nil)
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
func (b *Block) BaseFee() *uint256.Int {
	return executionArtefact(b, "baseFee", func(e *executionResults) *uint256.Int {
		i := uint256.Int(e.baseFee)
		return &i
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
