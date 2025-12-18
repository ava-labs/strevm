// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/holiman/uint256"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/txgossip"
)

func (vm *VM) newBlock(eth *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	return blocks.New(eth, parent, lastSettled, vm.log())
}

// ParseBlock parses the buffer as [rlp] encoding of a [types.Block]. It does
// NOT populate the block ancestry, which is done by [VM.VerifyBlock] i.f.f.
// verification passes.
func (vm *VM) ParseBlock(ctx context.Context, buf []byte) (*blocks.Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(buf, b); err != nil {
		return nil, fmt.Errorf("rlp.DecodeBytes(..., %T): %v", b, err)
	}
	return vm.newBlock(b, nil, nil)
}

// BuildBlock builds a new block, using the last block passed to
// [VM.SetPreference] as the parent. The block context MAY be nil.
func (vm *VM) BuildBlock(ctx context.Context, bCtx *block.Context) (*blocks.Block, error) {
	parent := vm.preference.Load()

	baseFee := uint256.NewInt(0) // TODO(arr4n) source this from the parent's worst-case *end* of execution
	filter := txpool.PendingFilter{
		BaseFee: baseFee,
	}
	return vm.buildBlock(ctx, bCtx, parent, vm.now(), vm.mempool.TransactionsByPriority(filter))
}

var errExecutionLagging = errors.New("execution lagging for settlement")

// buildBlock implements the block-building logic shared by [VM.BuildBlock] and
// [VM.VerifyBlock].
func (vm *VM) buildBlock(ctx context.Context, bCtx *block.Context, parent *blocks.Block, blockTime time.Time, candidates []*txgossip.LazyTransaction) (*blocks.Block, error) {
	log := vm.log().With(
		zap.Uint64("parent_height", parent.Height()),
		zap.Stringer("parent_hash", parent.Hash()),
		zap.Time("block_time", blockTime),
	)

	settleAt := unix(blockTime.Add(-params.Tau))
	lastSettled, ok, err := blocks.LastToSettleAt(settleAt, parent)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Warn("Execution lagging when determining last block to settle")
		return nil, errExecutionLagging
	}

	// Although, at the time of writing this, the implementation of
	// [blocks.LastToSettleAt] guarantees that the returned block has finished
	// execution, this isn't a stated invariant and it's possible for it to be
	// more aggressive in the future.
	if !lastSettled.Executed() {
		log.Warn(
			"Execution lagging for settlement artefacts",
			zap.Uint64("settling_height", lastSettled.Height()),
		)
		return nil, fmt.Errorf("%w: settling block %d: %T.WaitUntilExecuted(): %v", errExecutionLagging, lastSettled.Height(), lastSettled, err)
	}

	sdb, err := state.New(lastSettled.PostExecutionStateRoot(), vm.exec.StateCache(), nil)
	if err != nil {
		b := lastSettled
		return nil, fmt.Errorf("state.New([post-execution state root %#x of block %d], ...): %v", b.PostExecutionStateRoot(), b.Height(), err)
	}

	// TODO(arr4n) worst-case tx validation will happen here. This is just a
	// sketch of the process.
	history := parent.WhenChildSettles(lastSettled)
	var receipts types.Receipts
	for _, b := range history {
		receipts = append(receipts, b.Receipts()...)
	}

	clock := lastSettled.ExecutedByGasTime()
	for _, b := range history {
		clock.BeforeBlock(vm.hooks, b.Header())

		var lim gas.Gas
		for _, tx := range b.Transactions() {
			lim += gas.Gas(tx.Gas())
		}
		if err := clock.AfterBlock(lim, vm.hooks, b.Header()); err != nil {
			return nil, err
		}
	}

	hdr := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		Time:       unix(blockTime),
		Root:       lastSettled.PostExecutionStateRoot(),
	}
	clock.BeforeBlock(vm.hooks, hdr)
	hdr.BaseFee = clock.BaseFee().ToBig()

	var included []*types.Transaction
	for _, ltx := range candidates {
		tx, ok := ltx.Resolve()
		if !ok {
			continue
		}
		_ = sdb // balance checks
		included = append(included, tx)
	}

	ethB := types.NewBlock(
		hdr,
		included,
		nil, // uncles,
		receipts,
		trieHasher(),
	)
	return vm.newBlock(ethB, parent, lastSettled)
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

	switch height, accepted := b.Height(), vm.lastAccepted.Load().Height(); {
	case height != parent.Height()+1:
		return fmt.Errorf("non-incrementing block height; verifying at %d with parent at %d", height, parent.Height())
	case height <= accepted:
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

	rebuilt, err := vm.buildBlock(ctx, bCtx, parent, b.Timestamp(), txs)
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
