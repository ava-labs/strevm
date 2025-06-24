//go:build !prod && !nocmpopts

package blocks

import (
	"sync/atomic"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/saetest"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// CmpOpt returns a configuration for [cmp.Diff] to compare [Block] instances in
// tests.
func CmpOpt() cmp.Option {
	return cmp.Options{
		cmp.Comparer((*Block).equalForTests),
		saetest.CmpBigInts(),
		saetest.CmpTimes(),
		saetest.CmpReceiptsByMerkleRoot(),
		saetest.ComparerOptWithNilCheck(func(a, b *types.Block) bool {
			return a.Hash() == b.Hash()
		}),
		cmpopts.EquateComparable(
			// We're not running tests concurrently with anything that will
			// modify [Block.accepted] nor [Block.executed] so this is safe.
			// Using a [cmp.Transformer] would make the linter complain about
			// copying.
			atomic.Bool{},
		),
	}
}

func (b *Block) equalForTests(c *Block) bool {
	fn := saetest.ComparerWithNilCheck(func(b, c *Block) bool {
		if b.Hash() != c.Hash() {
			return false
		}

		fn := saetest.ComparerWithNilCheck(func(b, c *ancestry) bool {
			return b.parent.equalForTests(c.parent) && b.lastSettled.equalForTests(c.lastSettled)
		})
		if !fn(b.ancestry.Load(), c.ancestry.Load()) {
			return false
		}

		return b.execution.Load().equalForTests(c.execution.Load())
	})
	return fn(b, c)
}

func (e *executionResults) equalForTests(f *executionResults) bool {
	fn := saetest.ComparerWithNilCheck(func(e, f *executionResults) bool {
		return e.byGas.Cmp(f.byGas.Time) == 0 &&
			e.gasUsed == f.gasUsed &&
			e.receiptRoot == f.receiptRoot &&
			e.stateRootPost == f.stateRootPost
	})
	return fn(e, f)
}
