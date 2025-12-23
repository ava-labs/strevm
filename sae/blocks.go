// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/txgossip"
	"github.com/ava-labs/strevm/worstcase"
)

func (vm *VM) newBlock(eth *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	return blocks.New(eth, parent, lastSettled, vm.log())
}

var (
	errBlockHeightNotUint64 = errors.New("block height not uint64")
	errBlockTooFarInFuture  = errors.New("block too far in the future")
)

const maxBlockFutureSeconds = 3600

// ParseBlock parses the buffer as [rlp] encoding of a [types.Block]. It does
// NOT populate the block ancestry, which is done by [VM.VerifyBlock] i.f.f.
// verification passes.
func (vm *VM) ParseBlock(ctx context.Context, buf []byte) (*blocks.Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(buf, b); err != nil {
		return nil, fmt.Errorf("rlp.DecodeBytes(..., %T): %v", b, err)
	}

	if !b.Number().IsUint64() {
		return nil, errBlockHeightNotUint64
	}
	// The uint64 timestamp can't underflow [time.Time] but it can overflow so
	// make this some future engineer's problem in a few millennia.
	if b.Time() > unix(vm.config.Now())+maxBlockFutureSeconds {
		return nil, fmt.Errorf("%w: >%s", errBlockTooFarInFuture, maxBlockFutureSeconds*time.Second)
	}

	return vm.newBlock(b, nil, nil)
}

// BuildBlock builds a new block, using the last block passed to
// [VM.SetPreference] as the parent. The block context MAY be nil.
func (vm *VM) BuildBlock(ctx context.Context, bCtx *block.Context) (*blocks.Block, error) {
	return vm.buildBlock(
		ctx,
		bCtx,
		vm.preference.Load(),
		vm.config.Now(),
		vm.mempool.TransactionsByPriority,
		vm.hooks,
	)
}

var errExecutionLagging = errors.New("execution lagging for settlement")

// buildBlock implements the block-building logic shared by [VM.BuildBlock] and
// [VM.VerifyBlock].
func (vm *VM) buildBlock(
	ctx context.Context,
	bCtx *block.Context,
	parent *blocks.Block,
	blockTime time.Time,
	pendingTxs func(txpool.PendingFilter) []*txgossip.LazyTransaction,
	builder hook.BlockBuilder,
) (*blocks.Block, error) {
	log := vm.log().With(
		zap.Uint64("parent_height", parent.Height()),
		zap.Stringer("parent_hash", parent.Hash()),
		zap.Time("block_time", blockTime),
	)

	if blockTime.Before(parent.Timestamp()) {
		return nil, fmt.Errorf("block time %s < parent time %s", blockTime, parent.Timestamp())
	}
	// The block's time must be verified here to avoid underflow [unix].
	if blockTime.Unix() < params.TauSeconds {
		return nil, fmt.Errorf("block time %d < minimum allowed unix time %d", blockTime.Unix(), params.TauSeconds)
	}

	settleAt := unix(blockTime.Add(-params.Tau))
	lastSettled, ok, err := blocks.LastToSettleAt(settleAt, parent)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Warn("Execution lagging when determining last block to settle")
		return nil, errExecutionLagging
	}

	state, err := worstcase.NewState(vm.hooks, vm.exec.ChainConfig(), vm.exec.StateCache(), lastSettled)
	if err != nil {
		return nil, err
	}

	for _, b := range unsettledAncestry(lastSettled, parent) {
		if err := state.StartBlock(b.Header()); err != nil {
			return nil, fmt.Errorf("starting worst-case state for block %d: %v", b.Height(), err)
		}
		for _, tx := range b.Transactions() {
			if err := state.ApplyTx(tx); err != nil {
				return nil, fmt.Errorf("applying tx %#x in block %d to worst-case state: %v", tx.Hash(), b.Height(), err)
			}
		}
		for _, op := range vm.hooks.EndOfBlockOps(b.EthBlock()) {
			if err := state.Apply(op); err != nil {
				return nil, fmt.Errorf("applying op at end of block %d to worst-case state: %v", b.Height(), err)
			}
		}
		if err := state.FinishBlock(); err != nil {
			return nil, fmt.Errorf("finishing worst-case state for block %d: %v", b.Height(), err)
		}
	}

	hdr := &types.Header{
		ParentHash: parent.Hash(),
		Root:       lastSettled.PostExecutionStateRoot(),
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		// GasLimit is populated after [worstcase.State.StartBlock].
		// GasUsed should be populated after [worstcase.State.ApplyTx] has
		// finished being called.
		Time: unix(blockTime),
		// BaseFee is populated after [worstcase.State.StartBlock].

		// All other fields are, optionally, populated by the call to
		// [hook.BlockBuilder.BuildBlock] after applying every
		// [types.Transaction].
	}
	if err := state.StartBlock(hdr); err != nil {
		return nil, fmt.Errorf("starting worst-case state for new block: %v", err)
	}

	hdr.GasLimit = state.GasLimit()
	hdr.BaseFee = state.BaseFee().ToBig()

	var (
		candidates = pendingTxs(txpool.PendingFilter{
			BaseFee: state.BaseFee(),
		})
		included []*types.Transaction
	)
	for _, ltx := range candidates {
		tx, ok := ltx.Resolve()
		if !ok {
			continue
		}

		if err := state.ApplyTx(tx); err != nil {
			continue
		}
		included = append(included, tx)
	}

	// TODO: Should the [hook.BlockBuilder] populate [types.Header.GasUsed] so
	// that [hook.Op.Gas] can be included?
	hdr.GasUsed = state.GasUsed()

	// Although we never interact with the worst-case state after this point, we
	// still mark the block as finished to align with normal execution.
	if err := state.FinishBlock(); err != nil {
		return nil, fmt.Errorf("finishing worst-case state for new block: %v", err)
	}

	var receipts types.Receipts
	for _, b := range parent.WhenChildSettles(lastSettled) {
		receipts = append(receipts, b.Receipts()...)
	}

	ethB := builder.BuildBlock(
		hdr,
		included,
		receipts,
	)
	return vm.newBlock(ethB, parent, lastSettled)
}

// unsettledAncestry returns the ancestry of blocks from `parent` (inclusive) to
// (not including) `settled` in order of oldest to newest.
func unsettledAncestry(settled, parent *blocks.Block) []*blocks.Block {
	var history []*blocks.Block
	for b := parent; b.ID() != settled.ID(); b = b.ParentBlock() {
		history = append(history, b)
	}
	slices.Reverse(history)
	return history
}

// VerifyBlock validates the block and, if successful, populates its ancestry.
// The block context MAY be nil.
func (vm *VM) VerifyBlock(ctx context.Context, bCtx *block.Context, b *blocks.Block) error {
	// Sender caching should be performed as early as possible but not at the
	// risk of spam. [VM.ParseBlock] is too early as there is no protection,
	// whereas [VM.VerifyBlock] is only called after verifying the current
	// proposer's signature. While a malicious proposer could exist, their time
	// window is limited.
	signer := vm.signerForBlock(b.EthBlock())
	core.SenderCacher.Recover(signer, b.Transactions()) // asynchronous

	parent, err := vm.GetBlock(ctx, b.Parent())
	if err != nil {
		return fmt.Errorf("unknown block parent %#x: %w", b.ParentHash(), err)
	}

	// Sanity check that we aren't verifying an accepted block.
	if height, accepted := b.Height(), vm.lastAccepted.Load().Height(); height <= accepted {
		return fmt.Errorf("verifying block at height %d <= last-accepted (%d)", height, accepted)
	}

	txs := make([]*txgossip.LazyTransaction, len(b.Transactions()))
	for i, tx := range b.Transactions() {
		s, err := types.Sender(signer, tx)
		if err != nil {
			return fmt.Errorf("recovering sender of tx %#x: %v", tx.Hash(), err)
		}

		feeCap, err := uint256FromBig(tx.GasFeeCap())
		if err != nil {
			return fmt.Errorf("tx %#x fee cap: %v", tx.Hash(), err)
		}
		tipCap, err := uint256FromBig(tx.GasTipCap())
		if err != nil {
			return fmt.Errorf("tx %#x tip cap: %v", tx.Hash(), err)
		}

		txs[i] = &txgossip.LazyTransaction{
			LazyTransaction: &txpool.LazyTransaction{
				Hash:      tx.Hash(),
				Tx:        tx,
				GasFeeCap: feeCap,
				GasTipCap: tipCap,
				Gas:       tx.Gas(),
			},
			Sender: s,
		}
	}

	rebuilt, err := vm.buildBlock(
		ctx,
		bCtx,
		parent,
		b.Timestamp(),
		func(txpool.PendingFilter) []*txgossip.LazyTransaction { return txs },
		vm.hooks.BlockRebuilderFrom(b.EthBlock()),
	)
	if err != nil {
		return err
	}
	// Although this is also checked in [blocks.Block.CopyAncestorsFrom], it is
	// key to the purpose of this method so included here to be defensive. It
	// also provides a clearer failure message.
	if reH, verH := rebuilt.Hash(), b.Hash(); reH != verH {
		return fmt.Errorf("block-hash mismatch when rebuilding block; rebuilt as %#x when verifying %#x", reH, verH)
	}
	if err := b.CopyAncestorsFrom(rebuilt); err != nil {
		return err
	}

	vm.blocks.Store(b.Hash(), b)
	return nil
}

// GetBlock returns the block with the given ID, or [database.ErrNotFound].
func (vm *VM) GetBlock(ctx context.Context, id ids.ID) (*blocks.Block, error) {
	b, ok := vm.blocks.Load(common.Hash(id))
	if !ok {
		return nil, database.ErrNotFound
	}
	return b, nil
}

// GetBlockIDAtHeight returns the accepted block at the given height, or
// [database.ErrNotFound].
func (vm *VM) GetBlockIDAtHeight(context.Context, uint64) (ids.ID, error) {
	return ids.Empty, errUnimplemented
}

var _ blocks.Source = (*VM)(nil).blockSource

func (vm *VM) blockSource(hash common.Hash, num uint64) (*blocks.Block, bool) {
	b, ok := vm.blocks.Load(hash)
	if !ok || b.NumberU64() != num {
		return nil, false
	}
	return b, true
}
