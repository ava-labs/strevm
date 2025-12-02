// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package adaptor provides a generic alternative to the Snowman [block.ChainVM]
// interface, which doesn't require the block to be aware of the VM
// implementation.
package adaptor

import (
	"context"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
)

var (
	_ block.WithVerifyContext            = (*Block[BlockProperties])(nil)
	_ block.BuildBlockWithContextChainVM = (*adaptor[BlockProperties])(nil)
)

// ChainVM defines the functionality required in order to be converted into a
// Snowman VM. See the respective methods on [block.ChainVM] and [snowman.Block]
// for detailed documentation.
type ChainVM[BP BlockProperties] interface {
	common.VM

	GetBlock(context.Context, ids.ID) (BP, error)
	ParseBlock(context.Context, []byte) (BP, error)
	BuildBlock(context.Context) (BP, error)

	BuildBlockWithContext(context.Context, *block.Context) (BP, error)

	// Transferred from [snowman.Block].
	VerifyBlock(context.Context, BP) error
	AcceptBlock(context.Context, BP) error
	RejectBlock(context.Context, BP) error

	ShouldVerifyWithContext(context.Context, BP) (bool, error)
	VerifyWithContext(context.Context, *block.Context, BP) error

	SetPreference(context.Context, ids.ID) error
	LastAccepted(context.Context) (ids.ID, error)
	GetBlockIDAtHeight(context.Context, uint64) (ids.ID, error)
}

// BlockProperties is a read-only subset of [snowman.Block]. The state-modifying
// methods required by Snowman consensus are, instead, present on [ChainVM].
type BlockProperties interface {
	ID() ids.ID
	Parent() ids.ID
	Bytes() []byte
	Height() uint64
	Timestamp() time.Time
}

// Convert transforms a generic [ChainVM] into a standard [block.ChainVM]. All
// [snowman.Block] values returned by methods of the returned chain will be of
// the concrete type [Block] with type parameter `BP`.
func Convert[BP BlockProperties](vm ChainVM[BP]) block.ChainVM {
	return &adaptor[BP]{vm}
}

type adaptor[BP BlockProperties] struct {
	ChainVM[BP]
}

// Block is an implementation of [snowman.Block], used by chains returned by
// [Convert]. The [BlockProperties] can be accessed with [Block.Unwrap].
type Block[BP BlockProperties] struct {
	b  BP
	vm ChainVM[BP]
}

// Unwrap returns the [BlockProperties] carried by b.
func (b Block[BP]) Unwrap() BP { return b.b }

func (vm adaptor[BP]) newBlock(b BP, err error) (snowman.Block, error) {
	if err != nil {
		return nil, err
	}
	return Block[BP]{b, vm.ChainVM}, nil
}

func (vm adaptor[BP]) GetBlock(ctx context.Context, blkID ids.ID) (snowman.Block, error) {
	return vm.newBlock(vm.ChainVM.GetBlock(ctx, blkID))
}

func (vm adaptor[BP]) ParseBlock(ctx context.Context, blockBytes []byte) (snowman.Block, error) {
	return vm.newBlock(vm.ChainVM.ParseBlock(ctx, blockBytes))
}

func (vm adaptor[BP]) BuildBlock(ctx context.Context) (snowman.Block, error) {
	return vm.newBlock(vm.ChainVM.BuildBlock(ctx))
}

// BuildBlockWithContext calls BuildBlockWithContext(ctx, blockCtx) on the [ChainVM] that created b.
func (vm adaptor[BP]) BuildBlockWithContext(ctx context.Context, blockCtx *block.Context) (snowman.Block, error) {
	return vm.newBlock(vm.ChainVM.BuildBlockWithContext(ctx, blockCtx))
}

// Verify calls VerifyBlock(b) on the [ChainVM] that created b.
func (b Block[BP]) Verify(ctx context.Context) error { return b.vm.VerifyBlock(ctx, b.b) }

// Accept calls AcceptBlock(b) on the [ChainVM] that created b.
func (b Block[BP]) Accept(ctx context.Context) error { return b.vm.AcceptBlock(ctx, b.b) }

// Reject calls RejectBlock(b) on the [ChainVM] that created b.
func (b Block[BP]) Reject(ctx context.Context) error { return b.vm.RejectBlock(ctx, b.b) }

// ShouldVerifyWithContext calls ShouldVerifyWithContext(ctx, b.b) on the [ChainVM] that created b.
func (b Block[BP]) ShouldVerifyWithContext(ctx context.Context) (bool, error) {
	return b.vm.ShouldVerifyWithContext(ctx, b.b)
}

// VerifyWithContext calls VerifyWithContext(ctx, b.b) on the [ChainVM] that created b.
func (b Block[BP]) VerifyWithContext(ctx context.Context, blockCtx *block.Context) error {
	return b.vm.VerifyWithContext(ctx, blockCtx, b.b)
}

// ID propagates the respective method from the [BlockProperties] carried by b.
func (b Block[BP]) ID() ids.ID { return b.b.ID() }

// Parent propagates the respective method from the [BlockProperties] carried by b.
func (b Block[BP]) Parent() ids.ID { return b.b.Parent() }

// Bytes propagates the respective method from the [BlockProperties] carried by b.
func (b Block[BP]) Bytes() []byte { return b.b.Bytes() }

// Height propagates the respective method from the [BlockProperties] carried by b.
func (b Block[BP]) Height() uint64 { return b.b.Height() }

// Timestamp propagates the respective method from the [BlockProperties] carried by b.
func (b Block[BP]) Timestamp() time.Time { return b.b.Timestamp() }
