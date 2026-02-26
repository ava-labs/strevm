// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package hookstest provides a test double for SAE's [hook] package.
package hookstest

import (
	"iter"
	"math/big"
	"slices"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"

	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saetest"
)

// Stub implements [hook.PointsG].
type Stub struct {
	Now                     func() time.Time
	Target                  gas.Gas
	Ops                     []hook.Op
	ExecutionResultsDBFn    func(string) (saedb.ExecutionResults, error)
	CanExecuteTransactionFn func(common.Address, *common.Address, libevm.StateReader) error
}

var _ hook.PointsG[hook.Op] = (*Stub)(nil)

// ExecutionResultsDB propagates arguments to and from
// [Stub.ExecutionResultsDBFn] if non-nil, otherwise it returns a fresh
// [saetest.NewHeightIndexDB] on every call.
func (s *Stub) ExecutionResultsDB(dataDir string) (saedb.ExecutionResults, error) {
	if fn := s.ExecutionResultsDBFn; fn != nil {
		return fn(dataDir)
	}
	return saedb.ExecutionResults{
		HeightIndex: saetest.NewHeightIndexDB(),
	}, nil
}

// BuildHeader constructs a header that builds on top of the parent header. The
// `Extra` field SHOULD NOT be modified as it encodes sub-second block time.
func (s *Stub) BuildHeader(parent *types.Header) *types.Header {
	var now time.Time
	if s.Now != nil {
		now = s.Now()
	} else {
		now = time.Now()
	}

	canonicalExtra := extra{
		nanoseconds: time.Duration(now.Nanosecond()),
	}
	hdr := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		Time:       uint64(now.Unix()), //nolint:gosec // Known non-negative
		Extra:      canonicalExtra.MarshalCanoto(),
	}
	return hdr
}

// PotentialEndOfBlockOps returns [Stub.Ops] as a sequence.
func (s *Stub) PotentialEndOfBlockOps() iter.Seq[hook.Op] {
	return slices.Values(s.Ops)
}

// BuildBlock calls [BuildBlock] with its arguments.
func (*Stub) BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
	ops []hook.Op,
) (*types.Block, error) {
	return BuildBlock(header, txs, receipts, ops)
}

// BuildBlock encodes ops into [types.Header.Extra] and calls [types.NewBlock]
// with the other arguments.
func BuildBlock(
	header *types.Header,
	txs []*types.Transaction,
	receipts []*types.Receipt,
	ops []hook.Op,
) (*types.Block, error) {
	e := extra{}
	// If the header originally had fractional seconds set, we should keep them
	// in the built block.
	if err := e.UnmarshalCanoto(header.Extra); err != nil {
		return nil, err
	}

	e.ops = make([]op, len(ops))
	for i, hookOp := range ops {
		e.ops[i] = opFromHookOp(hookOp)
	}

	header.Extra = e.MarshalCanoto()
	return types.NewBlock(header, txs, nil, receipts, saetest.TrieHasher()), nil
}

// BlockRebuilderFrom returns a block builder that uses the provided block as a
// source of time.
func (s *Stub) BlockRebuilderFrom(b *types.Block) hook.BlockBuilder[hook.Op] {
	e := extra{}
	if err := e.UnmarshalCanoto(b.Extra()); err != nil {
		panic(err)
	}

	ops := make([]hook.Op, len(e.ops))
	for i, op := range e.ops {
		ops[i] = op.toHookOp()
	}
	return &Stub{
		Now: func() time.Time {
			return time.Unix(
				int64(b.Time()), //nolint:gosec // Won't overflow for a few millennia
				int64(e.nanoseconds),
			)
		},
		Ops: ops,
	}
}

// GasTargetAfter ignores its argument and always returns [Stub.Target].
func (s *Stub) GasTargetAfter(*types.Header) gas.Gas {
	return s.Target
}

// SubSecondBlockTime returns the sub-second time encoded and stored by
// [Stub.BuildHeader] in the header's `Extra` field. If said field is empty,
// SubSecondBlockTime returns 0.
func (s *Stub) SubSecondBlockTime(hdr *types.Header) time.Duration {
	canonicalExtra := extra{}
	if err := canonicalExtra.UnmarshalCanoto(hdr.Extra); err != nil {
		panic(err)
	}
	return canonicalExtra.nanoseconds
}

// EndOfBlockOps return the ops included in the block from [BuildBlock].
func (s *Stub) EndOfBlockOps(b *types.Block) []hook.Op {
	canonicalExtra := extra{}
	if err := canonicalExtra.UnmarshalCanoto(b.Extra()); err != nil {
		panic(err)
	}
	ops := make([]hook.Op, len(canonicalExtra.ops))
	for i, op := range canonicalExtra.ops {
		ops[i] = op.toHookOp()
	}
	return ops
}

// CanExecuteTransaction proxies to [Stub.CanExecuteTransactionFn] if non-nil,
// otherwise it allows all transactions.
func (s *Stub) CanExecuteTransaction(from common.Address, to *common.Address, sr libevm.StateReader) error {
	if fn := s.CanExecuteTransactionFn; fn != nil {
		return fn(from, to, sr)
	}
	return nil
}

// BeforeExecutingBlock is a no-op that always returns nil.
func (*Stub) BeforeExecutingBlock(params.Rules, *state.StateDB, *types.Block) error {
	return nil
}

// AfterExecutingBlock is a no-op.
func (*Stub) AfterExecutingBlock(*state.StateDB, *types.Block, types.Receipts) {}

//go:generate go run github.com/StephenButtolph/canoto/canoto $GOFILE

type extra struct {
	nanoseconds time.Duration `canoto:"int,1"`
	ops         []op          `canoto:"repeated value,2"`

	canotoData canotoData_extra
}

type op struct {
	op        ids.ID      `canoto:"fixed bytes,1"`
	gas       gas.Gas     `canoto:"uint,2"`
	gasFeeCap uint256.Int `canoto:"fixed repeated uint,3"`
	burn      []burn      `canoto:"repeated value,4"`
	mint      []mint      `canoto:"repeated value,5"`

	canotoData canotoData_op
}

func opFromHookOp(o hook.Op) op {
	op := op{
		op:        o.ID,
		gas:       o.Gas,
		gasFeeCap: o.GasFeeCap,
		burn:      make([]burn, 0, len(o.Burn)),
		mint:      make([]mint, 0, len(o.Mint)),
	}
	for addr, b := range o.Burn {
		op.burn = append(op.burn, burn{
			address: addr,
			nonce:   b.Nonce,
			amount:  b.Amount,
		})
	}
	for addr, amount := range o.Mint {
		op.mint = append(op.mint, mint{
			address: addr,
			amount:  amount,
		})
	}
	slices.SortFunc(op.burn, burn.Compare)
	slices.SortFunc(op.mint, mint.Compare)
	return op
}

func (o op) toHookOp() hook.Op {
	hookOp := hook.Op{
		ID:        o.op,
		Gas:       o.gas,
		GasFeeCap: o.gasFeeCap,
		Burn:      make(map[common.Address]hook.AccountDebit, len(o.burn)),
		Mint:      make(map[common.Address]uint256.Int, len(o.mint)),
	}
	for _, b := range o.burn {
		hookOp.Burn[b.address] = hook.AccountDebit{
			Nonce:  b.nonce,
			Amount: b.amount,
		}
	}
	for _, m := range o.mint {
		hookOp.Mint[m.address] = m.amount
	}
	return hookOp
}

type burn struct {
	address common.Address `canoto:"fixed bytes,1"`
	nonce   uint64         `canoto:"uint,2"`
	amount  uint256.Int    `canoto:"fixed repeated uint,3"`

	canotoData canotoData_burn
}

func (b burn) Compare(o burn) int {
	return b.address.Cmp(o.address)
}

type mint struct {
	address common.Address `canoto:"fixed bytes,1"`
	amount  uint256.Int    `canoto:"fixed repeated uint,3"`

	canotoData canotoData_mint
}

func (m mint) Compare(o mint) int {
	return m.address.Cmp(o.address)
}
