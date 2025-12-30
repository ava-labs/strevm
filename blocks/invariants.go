// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"fmt"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
)

type WorstCaseBounds struct {
	BaseFee          *uint256.Int
	TxSenderBalances []*uint256.Int
}

func (b *Block) SetWorstCaseBounds(lim *WorstCaseBounds) {
	b.bounds = lim
}

func (b *Block) WorstCaseBounds() *WorstCaseBounds {
	return b.bounds
}

func (lim *WorstCaseBounds) CheckBaseFee(log logging.Logger, actual *uint256.Int) {
	if lim.BaseFee.Lt(actual) {
		log.Error(
			"Predicted worst-case base fee < actual",
			zap.Stringer("predicted", lim.BaseFee),
			zap.Stringer("actual", actual),
		)
	}
}

func (lim *WorstCaseBounds) CheckSenderBalance(log logging.Logger, signer types.Signer, stateDB *state.StateDB, tx *types.Transaction) {
	sender, err := types.Sender(signer, tx)
	if err != nil {
		log.Warn(
			"Unable to recover sender for confirming worst-case balance",
			zap.Error(err),
		)
		return
	}

	bal := stateDB.GetBalance(sender)
	low := lim.TxSenderBalances[stateDB.TxIndex()]
	if low.Gt(bal) {
		log.Error(
			"Predicted worst-case balance > actual",
			zap.Stringer("sender", sender),
			zap.Stringer("predicted", low),
			zap.Stringer("actual", bal),
		)
	}
}

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
