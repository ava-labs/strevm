// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
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
		vm.hooks(),
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
	if hdr.Root != (common.Hash{}) || hdr.GasLimit != 0 || hdr.BaseFee != nil || hdr.GasUsed != 0 {
		log.Warn("Block builder returned header with at least one reserved field set",
			zap.Stringer("root", hdr.Root),
			zap.Uint64("gas_limit", hdr.GasLimit),
			zap.Stringer("base_fee", hdr.BaseFee),
			zap.Uint64("gas_used", hdr.GasUsed),
		)
	}

	bTime := blocks.PreciseTime(vm.hooks(), hdr)
	pTime := blocks.PreciseTime(vm.hooks(), parent.Header())

	// It is allowed for [hook.Points] to further constrain the allowed block
	// times. However, every block MUST at least satisfy these basic sanity
	// checks.
	if bTime.Unix() < saeparams.TauSeconds {
		return nil, fmt.Errorf("%w: %d < %d", errBlockTimeUnderMinimum, hdr.Time, saeparams.TauSeconds)
	}
	if bTime.Compare(pTime) < 0 {
		return nil, fmt.Errorf("%w: %s < %s", errBlockTimeBeforeParent, bTime.String(), pTime.String())
	}
	maxTime := vm.config.Now().Add(maxBlockFutureSeconds)
	if bTime.Compare(maxTime) > 0 {
		return nil, fmt.Errorf("%w: %s > %s", errBlockTimeAfterMaximum, bTime.String(), maxTime.String())
	}

	// Underflow of Add(-tau) is prevented by the above check.
	lastSettled, ok, err := blocks.LastToSettleAt(vm.hooks(), bTime.Add(-saeparams.Tau), parent)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Warn("Execution lagging when determining last block to settle")
		return nil, errExecutionLagging
	}

	log = log.With(
		zap.Uint64("last_settled_height", lastSettled.Height()),
		zap.Stringer("last_settled_hash", lastSettled.Hash()),
	)

	state, err := worstcase.NewState(vm.hooks(), vm.exec.ChainConfig(), vm.exec.StateCache(), lastSettled, vm.exec.SnapshotTree())
	if err != nil {
		log.Warn("Worst-case state not able to be created",
			zap.Error(err),
		)
		return nil, err
	}

	unsettled := blocks.Range(lastSettled, parent)
	for _, b := range unsettled {
		log := log.With(
			zap.Uint64("block_height", b.Height()),
			zap.Stringer("block_hash", b.Hash()),
		)
		if err := state.StartBlock(b.Header()); err != nil {
			log.Warn("Could not start historical worst-case calculation",
				zap.Error(err),
			)
			return nil, fmt.Errorf("starting worst-case state for block %d: %v", b.Height(), err)
		}
		for i, tx := range b.Transactions() {
			if err := state.ApplyTx(tx); err != nil {
				log.Warn("Could not apply tx during historical worst-case calculation",
					zap.Int("tx_index", i),
					zap.Stringer("tx_hash", tx.Hash()),
					zap.Error(err),
				)
				return nil, fmt.Errorf("applying tx %#x in block %d to worst-case state: %v", tx.Hash(), b.Height(), err)
			}
		}
		for i, op := range vm.hooks().EndOfBlockOps(b.EthBlock()) {
			if err := state.Apply(op); err != nil {
				log.Warn("Could not apply op during historical worst-case calculation",
					zap.Int("op_index", i),
					zap.Stringer("op_id", op.ID),
					zap.Error(err),
				)
				return nil, fmt.Errorf("applying op at end of block %d to worst-case state: %v", b.Height(), err)
			}
		}
		if _, err := state.FinishBlock(); err != nil {
			log.Warn("Could not finish historical worst-case calculation",
				zap.Error(err),
			)
			return nil, fmt.Errorf("finishing worst-case state for block %d: %v", b.Height(), err)
		}
	}

	hdr.Root = lastSettled.PostExecutionStateRoot()
	if err := state.StartBlock(hdr); err != nil {
		// A full queue is a normal mode of operation (backpressure working as
		// intended) so should not be a warning.
		logTo := log.Warn
		if errors.Is(err, worstcase.ErrQueueFull) {
			logTo = log.Debug
		}
		logTo("Could not start worst-case block calculation",
			zap.Error(err),
		)
		return nil, fmt.Errorf("starting worst-case state for new block: %w", err)
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
		log = log.With(
			zap.Stringer("tx_hash", ltx.Hash),
			zap.Int("tx_index", len(included)),
			zap.Stringer("sender", ltx.Sender),
		)

		tx, ok := ltx.Resolve()
		if !ok {
			log.Debug("Could not resolve lazy transaction")
			continue
		}

		// The [saexec.Executor] checks the worst-case balance before tx
		// execution so we MUST record it at the equivalent point, before
		// ApplyTx().
		if err := state.ApplyTx(tx); err != nil {
			log.Debug("Could not apply transaction", zap.Error(err))
			continue
		}
		log.Trace("Including transaction")
		included = append(included, tx)
	}

	var receipts types.Receipts
	settling := blocks.Range(parent.LastSettled(), lastSettled)
	for _, b := range settling {
		receipts = append(receipts, b.Receipts()...)
	}

	// BuildBlock populates hdr.GasUsed including end-of-block op gas.
	ethB, err := builder.BuildBlock(
		hdr,
		included,
		receipts,
	)
	if err != nil {
		return nil, err
	}

	// Apply end-of-block ops to worst-case state, mirroring the historical
	// block loop above. This must happen before FinishBlock so that the
	// queue size and MinOpBurnerBalances include op gas.
	for i, op := range vm.hooks().EndOfBlockOps(ethB) {
		if err := state.Apply(op); err != nil {
			log.Warn("Could not apply op during worst-case calculation",
				zap.Int("op_index", i),
				zap.Stringer("op_id", op.ID),
				zap.Error(err),
			)
			return nil, fmt.Errorf("applying op at end of new block to worst-case state: %v", err)
		}
	}

	bounds, err := state.FinishBlock()
	if err != nil {
		log.Warn("Could not finish worst-case block calculation",
			zap.Error(err),
		)
		return nil, fmt.Errorf("finishing worst-case state for new block: %v", err)
	}

	b, err := vm.newBlock(ethB, parent, lastSettled)
	if err != nil {
		return nil, err
	}
	b.SetWorstCaseBounds(bounds)
	return b, nil
}

var (
	errUnknownParent     = errors.New("unknown parent")
	errBlockHeightTooLow = errors.New("block height too low")
	errHashMismatch      = errors.New("hash mismatch")
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
	if height, accepted := b.Height(), vm.last.accepted.Load().Height(); height <= accepted {
		return fmt.Errorf("%w at height %d <= last-accepted (%d)", errBlockHeightTooLow, height, accepted)
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
		vm.hooks().BlockRebuilderFrom(b.EthBlock()),
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
	b.SetWorstCaseBounds(rebuilt.WorstCaseBounds())

	vm.blocks.Store(b.Hash(), b)
	return nil
}

func canonicalBlock(db ethdb.Database, num uint64) (*types.Block, error) {
	b := rawdb.ReadBlock(db, rawdb.ReadCanonicalHash(db, num), num)
	if b == nil {
		return nil, fmt.Errorf("no canonical block at height %d", num)
	}
	return b, nil
}

// GetBlock returns the block with the given ID, or [database.ErrNotFound].
//
// It is expected that blocks that have been successfully verified should be
// returned correctly. It is also expected that blocks that have been
// accepted by the consensus engine should be able to be fetched. It is not
// required for blocks that have been rejected by the consensus engine to be
// able to be fetched.
func (vm *VM) GetBlock(ctx context.Context, id ids.ID) (*blocks.Block, error) {
	var _ snowman.Block // protect the input to allow comment linking

	return readByHash(
		vm,
		common.Hash(id),
		func(b *blocks.Block) *blocks.Block {
			return b
		},
		func(db ethdb.Reader, hash common.Hash, num uint64) (*blocks.Block, error) {
			// A block that's not in memory has either been rejected, not yet
			// verified, or settled. Of these, only the latter would be in the
			// database.
			//
			// There is, however, a negligible (read: near impossible) but
			// non-zero chance that [VM.VerifyBlock] and [VM.AcceptBlock] were
			// *both* called between [readByHash] checking the in-memory block
			// store and loading the canonical number from the database. That
			// could result in an unexecuted block, which would cause an error
			// when restoring it.
			//
			// TODO(arr4n) I think [readHash] should be providing this guarantee
			// as it has access to the [syncMap] and its lock.
			if vm.last.settled.Load().Height() < num {
				return nil, database.ErrNotFound
			}

			ethB := rawdb.ReadBlock(db, hash, num)
			if num > vm.last.synchronous {
				return blocks.RestoreSettledBlock(
					ethB,
					vm.log(),
					vm.db,
					vm.xdb,
					vm.exec.ChainConfig(),
				)
			}

			b, err := vm.newBlock(ethB, nil, nil)
			if err != nil {
				return nil, err
			}
			// Excess is only used for executing the next block, which can never
			// be the case if `b` isn't actually the last synchronous block, so
			// passing the same value for all is OK.
			if err := b.MarkSynchronous(vm.hooks(), vm.db, vm.xdb, vm.config.ExcessAfterLastSynchronous); err != nil {
				return nil, err
			}
			return b, nil
		},
		database.ErrNotFound,
	)
}

// GetBlockIDAtHeight returns the accepted block at the given height, or
// [database.ErrNotFound].
func (vm *VM) GetBlockIDAtHeight(ctx context.Context, height uint64) (ids.ID, error) {
	id := ids.ID(rawdb.ReadCanonicalHash(vm.db, height))
	if id == ids.Empty {
		return id, database.ErrNotFound
	}
	return id, nil
}

var (
	_ blocks.EthBlockSource = (*VM)(nil).ethBlockSource
	_ blocks.HeaderSource   = (*VM)(nil).headerSource
)

func (vm *VM) ethBlockSource(hash common.Hash, num uint64) (*types.Block, bool) {
	return source(vm, hash, num, (*blocks.Block).EthBlock, rawdb.ReadBlock)
}

func (vm *VM) headerSource(hash common.Hash, num uint64) (*types.Header, bool) {
	return source(vm, hash, num, (*blocks.Block).Header, rawdb.ReadHeader)
}

func source[T any](vm *VM, hash common.Hash, num uint64, fromMem blockAccessor[T], fromDB canonicalReader[T]) (*T, bool) {
	if b, ok := vm.blocks.Load(hash); ok {
		if b.NumberU64() != num {
			return nil, false
		}
		return fromMem(b), true
	}
	x := fromDB(vm.db, hash, num)
	return x, x != nil
}
