package sae

import (
	"github.com/ava-labs/strevm/blocks"
	"go.uber.org/zap"
)

// trieDBCommitBlockIntervalLog2 is the base-2 log of the number of blocks
// between trie DB root commitments. The root is commited to disk if the height
// is a multiple of this value, which MUST NOT be modified outside of tests.
var trieDBCommitBlockIntervalLog2 uint64 = 12 // 4096

func trieDBCommitIntervalMask() uint64 {
	return (1 << trieDBCommitBlockIntervalLog2) - 1
}

func shouldCommitTrieDB(height uint64) bool {
	return height&trieDBCommitIntervalMask() == 0
}

func lastTrieDBCommittedAt(lastAccepted uint64) uint64 {
	return lastAccepted & (^trieDBCommitIntervalMask())
}

func (vm *VM) maybeCommitTrieDB(b *blocks.Block) error {
	if !shouldCommitTrieDB(b.Height()) {
		return nil
	}
	if err := vm.exec.StateCache().TrieDB().Commit(b.Root(), false); err != nil {
		return err
	}
	vm.logger().Info(
		"State root committed to disk",
		zap.Stringer("root", b.Root()),
		zap.Uint64("accepted_block", b.Height()),
		zap.Uint64("settled_block", b.LastSettled().Height()),
	)
	return nil
}
