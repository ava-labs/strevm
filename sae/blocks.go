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
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/txgossip"
)

// ParseBlock parses the buffer as [rlp] encoding of a [types.Block]. It does
// NOT populate the block ancestry, which is done by [VM.VerifyBlock] i.f.f.
// verification passes.
func (vm *VM) ParseBlock(ctx context.Context, buf []byte) (*blocks.Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(buf, b); err != nil {
		return nil, fmt.Errorf("rlp.DecodeBytes(..., %T): %v", b, err)
	}
	core.SenderCacher.Recover(vm.signerForBlock(b), b.Transactions())
	return blocks.New(b, nil, nil, vm.log())
}

// BuildBlock builds a new block, using the last block passed to
// [VM.SetPreference] as the parent.
func (vm *VM) BuildBlock(ctx context.Context) (*blocks.Block, error) {
	parent := vm.preference.Load()

	baseFee := uint256.NewInt(0) // TODO(arr4n) source this from the parent's worst-case *end* of execution
	filter := txpool.PendingFilter{
		BaseFee: baseFee,
	}
	return vm.buildBlock(ctx, parent, vm.now(), vm.mempool.TransactionsByPriority(filter))
}

// VerifyBlock validates the block and, if successful, populates its ancestry.
func (vm *VM) VerifyBlock(ctx context.Context, b *blocks.Block) error {
	parent, ok := vm.blocks.Load(b.ParentHash())
	if !ok {
		return fmt.Errorf("unknown block parent %#x", b.ParentHash())
	}
	if b.Height() != parent.Height()+1 {
		return fmt.Errorf("non-incrementing block height; building at %d with parent at %d", b.Height(), parent.Height())
	}
	if parent.Settled() && !parent.Synchronous() {
		// If the parent is settled then it MUST have some descendant block that
		// was already accepted by consensus. There is therefore no reason to
		// allow `b` into consensus, and doing so would break invariants
		// required by, for example, [blocks.LastToSettleAt].
		return fmt.Errorf("verifying block with settled parent %#x", b.ParentHash())
	}

	signer := vm.signerForBlock(b.EthBlock())
	txs := make([]*txgossip.LazyTransaction, len(b.Transactions()))
	for i, tx := range b.Transactions() {
		s, err := types.Sender(signer, tx)
		if err != nil {
			return fmt.Errorf("recovering sender of tx %#x: %v", tx.Hash(), err)
		}

		feeCap, err := uint256FromBig(tx.GasFeeCap(), errOnNil)
		if err != nil {
			return fmt.Errorf("tx %#x fee cap: %v", tx.Hash(), err)
		}
		tipCap, err := uint256FromBig(tx.GasTipCap(), errOnNil)
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

	rebuilt, err := vm.buildBlock(ctx, parent, b.Timestamp(), txs)
	switch err := err.(type) {
	case errDeterminingLastBlockToSettle:
		vm.log().Warn(
			"Potentially malicious block being validated",
			zap.Error(err.err),
		)
		return err
	case nil:
	default:
		return err
	}
	// Although check is also performed in [blocks.Block.CopyAncestorsFrom], it
	// is key to the purpose of this method so included here to be defensive.
	// It also provides a clearer failure message.
	if reH, verH := rebuilt.Hash(), b.Hash(); reH != verH {
		return fmt.Errorf("block-hash mismatch when rebuilding block; rebuilt as %#x when verifying %#x", reH, verH)
	}
	if err := b.CopyAncestorsFrom(rebuilt); err != nil {
		return err
	}

	vm.blocks.Store(b.Hash(), b)
	return nil
}

var errExecutionLagging = errors.New("execution lagging for settlement")

type errDeterminingLastBlockToSettle struct {
	err error
}

func (err errDeterminingLastBlockToSettle) Error() string {
	return fmt.Sprintf("broken invariant when determining last block to settle: %v", err.err)
}

func (vm *VM) buildBlock(ctx context.Context, parent *blocks.Block, blockTime time.Time, candidates []*txgossip.LazyTransaction) (*blocks.Block, error) {
	settleAt := unix(blockTime.Add(-params.Tau))
	lastSettled, ok, err := blocks.LastToSettleAt(settleAt, parent)
	if err != nil {
		return nil, errDeterminingLastBlockToSettle{err}
	}
	if !ok {
		return nil, errExecutionLagging
	}
	history := parent.WhenChildSettles(lastSettled)
	var receipts types.Receipts
	for _, b := range history {
		receipts = append(receipts, b.Receipts()...)
	}

	// Although, at the time of writing this, the implementation of
	// [blocks.LastToSettleAt] guarantees that the returned block has finished
	// execution, this isn't a stated invariant and it's possible for it to be
	// more aggressive. We SHOULD NOT make it an invariant as we can do
	// meaningful work in the interim (or choose to block here instead, for a
	// shorter period).
	wCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if err := lastSettled.WaitUntilExecuted(wCtx); err != nil {
		return nil, fmt.Errorf("%w: settling block %d: %T.WaitUntilExecuted(): %v", errExecutionLagging, lastSettled.Height(), lastSettled, err)
	}

	sdb, err := state.New(lastSettled.PostExecutionStateRoot(), vm.exec.StateCache(), nil)
	if err != nil {
		b := lastSettled
		return nil, fmt.Errorf("state.New([post-execution state root %#x of block %d], ...): %v", b.PostExecutionStateRoot(), b.Height(), err)
	}

	// TODO(arr4n) worst-case tx validation will happen here. This is just a
	// sketch of the process.
	clock := lastSettled.ExecutedByGasTime()
	for _, b := range history {
		// TODO(arr4n) modify [hook.BeforeBlock] and [hook.AfterBlock] to
		// differentiate between building/verification and execution.
		if err := hook.BeforeBlock(vm.hooks, vm.rulesForBlock(b.EthBlock()), sdb, b, clock); err != nil {
			return nil, fmt.Errorf("hook.BeforeBlock(): %v", err)
		}
		var consumed gas.Gas
		for _, tx := range b.Transactions() {
			g := gas.Gas(tx.Gas())
			clock.Tick(g)
			consumed += g
		}
		hook.AfterBlock(vm.hooks, sdb, b.EthBlock(), clock, consumed, nil)
	}
	baseFee := clock.BaseFee()
	var included []*types.Transaction
	for _, ltx := range candidates {
		tx := ltx.Resolve()
		if tx == nil { // foot gun!
			continue
		}
		_ = sdb // balance checks
		included = append(included, tx)
	}

	ethB := types.NewBlock(
		&types.Header{
			ParentHash: parent.Hash(),
			Number:     new(big.Int).Add(parent.Number(), big.NewInt(1)),
			Root:       lastSettled.PostExecutionStateRoot(),
			Time:       unix(blockTime),
			BaseFee:    baseFee.ToBig(),
		},
		included,
		nil, // uncles,
		receipts,
		trieHasher(),
	)
	return blocks.New(ethB, parent, lastSettled, vm.log())
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
