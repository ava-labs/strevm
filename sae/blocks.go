// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	saeparams "github.com/ava-labs/strevm/params"
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
		vm.mempool.TransactionsByPriority,
		vm.hooks,
	)
}

// Max time from current time allowed for blocks, before they're considered
// future blocks and fail verification.
const maxFutureBlockTime = 10 * time.Second

var (
	errBlockTimeUnderMinimum = errors.New("block time under minimum allowed time")
	errBlockTimeBeforeParent = errors.New("block time before parent time")
	errBlockTimeAfterMaximum = errors.New("block time after maximum allowed time")
	errExecutionLagging      = errors.New("execution lagging for settlement")
)

// buildBlock implements the block-building logic shared by [VM.BuildBlock] and
// [VM.VerifyBlock].
func (vm *VM) buildBlock(
	ctx context.Context,
	bCtx *block.Context,
	parent *blocks.Block,
	pendingTxs func(txpool.PendingFilter) []*txgossip.LazyTransaction,
	builder hook.BlockBuilder,
) (*blocks.Block, error) {
	hdr := builder.BuildHeader(parent.Header())
	log := vm.log().With(
		zap.Uint64("parent_height", parent.Height()),
		zap.Stringer("parent_hash", parent.Hash()),
		zap.Uint64("block_time", hdr.Time),
	)

	// It is allowed for [hook.Points] to further constrain the allowed block
	// times. However, every block MUST at least satisfy these basic sanity
	// checks.
	if hdr.Time < saeparams.TauSeconds {
		return nil, fmt.Errorf("%w: %d < %d", errBlockTimeUnderMinimum, hdr.Time, saeparams.TauSeconds)
	}
	if parentTime := parent.BuildTime(); hdr.Time < parentTime {
		return nil, fmt.Errorf("%w: %d < %d", errBlockTimeBeforeParent, hdr.Time, parentTime)
	}
	if maxTime := uint64(vm.config.Now().Add(maxFutureBlockTime).Unix()); hdr.Time > maxTime { //nolint:gosec // Time won't overflow for quite a while
		return nil, fmt.Errorf("%w: %d > %d", errBlockTimeAfterMaximum, hdr.Time, maxTime)
	}

	// TODO(StephenButtolph) settlement logic needs to support sub-second block
	// times.
	settleAt := hdr.Time - saeparams.TauSeconds
	lastSettled, ok, err := blocks.LastToSettleAt(settleAt, parent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errExecutionLagging
	}

	log = log.With(
		zap.Uint64("last_settled_height", lastSettled.Height()),
		zap.Stringer("last_settled_hash", lastSettled.Hash()),
	)

	state, err := worstcase.NewState(vm.hooks, vm.exec.ChainConfig(), vm.exec.StateCache(), lastSettled)
	if err != nil {
		log.Warn("Settled state not available")
		return nil, err
	}

	for _, b := range unsettledAncestry(parent, lastSettled.Height()) {
		log := log.With(
			zap.Uint64("block_height", b.Height()),
			zap.Stringer("block_hash", b.Hash()),
		)
		if err := state.StartBlock(b.Header()); err != nil {
			log.Warn("Could not start historical worst case calculation",
				zap.Error(err),
			)
			return nil, fmt.Errorf("starting worst-case state for block %d: %v", b.Height(), err)
		}
		for i, tx := range b.Transactions() {
			if err := state.ApplyTx(tx); err != nil {
				log.Warn("Could not apply tx during historical worst case calculation",
					zap.Int("tx_index", i),
					zap.Stringer("tx_hash", tx.Hash()),
					zap.Error(err),
				)
				return nil, fmt.Errorf("applying tx %#x in block %d to worst-case state: %v", tx.Hash(), b.Height(), err)
			}
		}
		for i, op := range vm.hooks.EndOfBlockOps(b.EthBlock()) {
			if err := state.Apply(op); err != nil {
				log.Warn("Could not apply op during historical worst case calculation",
					zap.Int("op_index", i),
					zap.Stringer("op_id", op.ID),
					zap.Error(err),
				)
				return nil, fmt.Errorf("applying op at end of block %d to worst-case state: %v", b.Height(), err)
			}
		}
		if err := state.FinishBlock(); err != nil {
			log.Warn("Could not finish historical worst case calculation",
				zap.Error(err),
			)
			return nil, fmt.Errorf("finishing worst-case state for block %d: %v", b.Height(), err)
		}
	}

	hdr.Root = lastSettled.PostExecutionStateRoot()
	if err := state.StartBlock(hdr); err != nil {
		log.Warn("Could not start worst case block calculation",
			zap.Error(err),
		)
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
		// If we don't have enough gas remaining in the block for the minimum
		// gas amount, we are done including transactions.
		if remainingGas := state.GasLimit() - state.GasUsed(); remainingGas < params.TxGas {
			break
		}

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
		log.Warn("Could not finish worst case block calculation",
			zap.Error(err),
		)
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
// the (non-inclusive) `settledHeight`; in order of oldest to newest.
//
// It is assumed that `parent` was successfully verified and has not been
// rejected.
func unsettledAncestry(parent *blocks.Block, settledHeight uint64) []*blocks.Block {
	parentHeight := parent.Height()
	if parentHeight <= settledHeight {
		return nil
	}

	history := make([]*blocks.Block, parentHeight-settledHeight)
	for i := range history {
		history[i] = parent
		parent = parent.ParentBlock()
	}
	slices.Reverse(history)
	return history
}

var (
	errUnknownParent  = errors.New("unknown parent")
	errFinalizedBlock = errors.New("finalized block")
	errHashMismatch   = errors.New("hash mismatch")
)

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
		return fmt.Errorf("%w %#x: %w", errUnknownParent, b.ParentHash(), err)
	}

	// Sanity check that we aren't verifying an accepted block.
	if height, accepted := b.Height(), vm.lastAccepted.Load().Height(); height <= accepted {
		return fmt.Errorf("%w at height %d <= last-accepted (%d)", errFinalizedBlock, height, accepted)
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
		func(f txpool.PendingFilter) []*txgossip.LazyTransaction { return txs },
		vm.hooks.BlockRebuilderFrom(b.EthBlock()),
	)
	if err != nil {
		return err
	}
	// Although this is also checked in [blocks.Block.CopyAncestorsFrom], it is
	// key to the purpose of this method so included here to be defensive. It
	// also provides a clearer failure message.
	if reH, verH := rebuilt.Hash(), b.Hash(); reH != verH {
		return fmt.Errorf("%w; rebuilt as %#x when verifying %#x", errHashMismatch, reH, verH)
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
