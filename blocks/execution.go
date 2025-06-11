package blocks

import (
	"fmt"
	"slices"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/gastime"
)

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

type executionResults struct {
	byGas  gastime.Time `canoto:"value,1"`
	byWall time.Time    // For metrics only; allowed to be incorrect.

	// Receipts are deliberately not stored by the canoto representation as they
	// are already in the database. Only [Block.RestorePostExecutionState] reads
	// the stored canoto, also accepting a [types.Receipts] argument that it
	// checks against `receiptRoot`.
	receipts      types.Receipts
	receiptRoot   common.Hash `canoto:"fixed bytes,2"`
	gasUsed       gas.Gas     `canoto:"uint,3"`
	stateRootPost common.Hash `canoto:"fixed bytes,4"`

	canotoData canotoData_executionResults
}

// Equal MUST NOT be used other than in [Block.Equal].
func (e *executionResults) Equal(f *executionResults) bool {
	if en, fn := e == nil, f == nil; en == true && fn == true {
		return true
	} else if en != fn {
		return false
	}

	return e.byGas.Cmp(f.byGas.Time) == 0 &&
		e.gasUsed == f.gasUsed &&
		e.receiptRoot == f.receiptRoot &&
		e.stateRootPost == f.stateRootPost
}

// MarkExecuted marks the block as having being executed at the specified
// time(s) and with the specified results. This function MUST NOT be called more
// than once. The wall-clock [time.Time] is for metrics only.
//
// Note: only in-memory state is updated. Receipts SHOULD be stored
// independently of a call to MarkExecuted, along with a call to
// [Block.WritePostExecutionState].
func (b *Block) MarkExecuted(byGas *gastime.Time, byWall time.Time, receipts types.Receipts, stateRootPost common.Hash) error {
	var used gas.Gas
	for _, r := range receipts {
		used += gas.Gas(r.GasUsed)
	}

	e := &executionResults{
		byGas:         *byGas.Clone(),
		byWall:        byWall,
		receipts:      slices.Clone(receipts),
		gasUsed:       used,
		receiptRoot:   types.DeriveSha(receipts, trie.NewStackTrie(nil)),
		stateRootPost: stateRootPost,
	}
	if !b.execution.CompareAndSwap(nil, e) {
		b.log.Error("Block re-marked as executed")
		return fmt.Errorf("block %d re-marked as executed", b.Height())
	}
	return nil
}

// Executed reports whether [Block.MarkExecuted] has been called.
func (b *Block) Executed() bool {
	return b.execution.Load() != nil
}

func zero[T any]() (z T) { return }

// ExecutedByGasTime returns a clone of the gas time passed to
// [Block.MarkExecuted] or nil if no such call has been made.
func (b *Block) ExecutedByGasTime() *gastime.Time {
	if e := b.execution.Load(); e != nil {
		return e.byGas.Clone()
	}
	b.log.Error("Get block execution (gas) time before execution")
	return nil
}

// ExecutedByWallTime returns the wall time passed to [Block.MarkExecuted] or
// the zero time if no such call has been made.
func (b *Block) ExecutedByWallTime() time.Time {
	if e := b.execution.Load(); e != nil {
		return e.byWall
	}
	b.log.Error("Get block execution (wall) time before execution")
	return zero[time.Time]()
}

// Receipts returns the receipts passed to [Block.MarkExecuted] or nil if no
// such call has been made.
func (b *Block) Receipts() types.Receipts {
	if e := b.execution.Load(); e != nil {
		return slices.Clone(e.receipts)
	}
	b.log.Error("Get block receipts before execution")
	return nil
}

// PostExecutionStateRoot returns the state root passed to [Block.MarkExecuted]
// or the zero hash if no such call has been made.
func (b *Block) PostExecutionStateRoot() common.Hash {
	if e := b.execution.Load(); e != nil {
		return e.stateRootPost
	}
	b.log.Error("Get block state root before execution")
	return zero[common.Hash]()
}
