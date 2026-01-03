// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"fmt"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
)

// A LifeCycleStage defines the progression of a block from acceptance through
// to settlement.
type LifeCycleStage int

// Valid [LifeCycleStage] values. Blocks proceed in increasing stage numbers,
// but specific values MUST NOT be relied upon to be stable.
const (
	NotExecuted LifeCycleStage = iota
	Executed
	Settled

	Accepted = NotExecuted
)

func (b *Block) brokenInvariantErr(msg string) error {
	return fmt.Errorf("block %d: %s", b.Height(), msg)
}

// CheckInvariants checks internal invariants against expected stage, typically
// only used during database recovery.
func (b *Block) CheckInvariants(expect LifeCycleStage) error {
	switch e := b.execution.Load(); e {
	case nil: // not executed
		if expect >= Executed {
			return b.brokenInvariantErr("expected to be executed")
		}
	default: // executed
		if expect < Executed {
			return b.brokenInvariantErr("unexpectedly executed")
		}
		if e.receiptRoot != types.DeriveSha(e.receipts, trie.NewStackTrie(nil)) {
			return b.brokenInvariantErr("receipts don't match root")
		}
	}

	switch a := b.ancestry.Load(); a {
	case nil: // settled
		if expect < Settled {
			return b.brokenInvariantErr("unexpectedly settled")
		}
	default: // not settled
		if expect >= Settled {
			return b.brokenInvariantErr("expected to be settled")
		}
		if b.SettledStateRoot() != b.LastSettled().PostExecutionStateRoot() {
			return b.brokenInvariantErr("state root does not match last-settled post execution")
		}
	}

	return nil
}
