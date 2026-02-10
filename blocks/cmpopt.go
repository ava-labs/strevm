// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build !prod && !nocmpopts

package blocks

import (
	"sync/atomic"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ava-labs/strevm/cmputils"
	"github.com/ava-labs/strevm/saetest"
)

// CmpOpt returns a configuration for [cmp.Diff] to compare [Block] instances in
// tests.
func CmpOpt() cmp.Option {
	return cmp.Comparer((*Block).equalForTests)
	return cmp.Options{
		cmpopts.EquateEmpty(),
		cmp.AllowUnexported(Block{}, ancestry{}),
		cmpopts.IgnoreFields(
			Block{},
			"bounds",
			"interimExecutionTime",
			"log",
		),
		cmputils.IfIn[Block](cmpopts.IgnoreTypes(make(chan struct{}))),
		cmp.Comparer((*executionResults).equalForTests),
		cmputils.Blocks(),
		cmputils.Headers(),
		cmp.Transformer("ancestry", func(p atomic.Pointer[ancestry]) *ancestry {
			return p.Load()
		}),
		cmp.Transformer("execution", func(p atomic.Pointer[executionResults]) *executionResults {
			return p.Load()
		}),
	}
}

func (b *Block) equalForTests(c *Block) bool {
	fn := cmputils.WithNilCheck(func(b, c *Block) bool {
		return true &&
			b.Hash() == c.Hash() &&
			b.ancestry.Load().equalForTests(c.ancestry.Load()) &&
			b.execution.Load().equalForTests(c.execution.Load())
	})
	return fn(b, c)
}

func (a *ancestry) equalForTests(b *ancestry) bool {
	fn := cmputils.WithNilCheck(func(a, b *ancestry) bool {
		return true &&
			a.parent.equalForTests(b.parent) &&
			a.lastSettled.equalForTests(b.lastSettled)
	})
	return fn(a, b)
}

func (e *executionResults) equalForTests(f *executionResults) bool {
	fn := cmputils.WithNilCheck(func(e, f *executionResults) bool {
		return true &&
			e.byGas.Rate() == f.byGas.Rate() &&
			e.byGas.Compare(f.byGas.Time) == 0 &&
			e.receiptRoot == f.receiptRoot &&
			saetest.MerkleRootsEqual(e.receipts, f.receipts) &&
			e.stateRootPost == f.stateRootPost
	})
	return fn(e, f)
}
