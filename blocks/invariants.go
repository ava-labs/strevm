package blocks

import (
	"fmt"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
)

type brokenInvariant struct {
	b   *Block
	msg string
}

func (err brokenInvariant) Error() string {
	return fmt.Sprintf("block %d: %s", err.b.Height(), err.msg)
}

// CheckInvariants checks internal invariants against expected state, typically
// only necessary during database recovery.
func (b *Block) CheckInvariants(expectExecuted, expectSettled bool) error {
	switch e := b.execution.Load(); e {
	case nil: // not executed
		if expectExecuted {
			return b.brokenInvariantErr("expected to be executed")
		}
	default: // executed
		if !expectExecuted {
			return b.brokenInvariantErr("unexpectedly executed")
		}
		if e.receiptRoot != types.DeriveSha(e.receipts, trie.NewStackTrie(nil)) {
			return b.brokenInvariantErr("receipts don't match root")
		}
	}

	switch a := b.ancestry.Load(); a {
	case nil: // settled
		if !expectSettled {
			return b.brokenInvariantErr("unexpectedly settled")
		}
	default: // not settled
		if expectSettled {
			return b.brokenInvariantErr("expected to be settled")
		}
		if b.SettledStateRoot() != b.LastSettled().PostExecutionStateRoot() {
			return b.brokenInvariantErr("state root does not match last-settled post execution")
		}
	}

	return nil
}

func (b *Block) brokenInvariantErr(msg string) error {
	return brokenInvariant{b: b, msg: msg}
}
