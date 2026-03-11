// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpc

import (
	"context"
	"errors"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
)

func (b *apiBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	receipts, _, err := b.getReceipts(rpc.BlockNumberOrHashWithHash(hash, false))
	if err != nil {
		return nil, nil //nolint:nilerr // This follows Geth behavior for [ethapi.Backend.GetReceipts]
	}
	return receipts, nil
}

// getReceipts resolves receipts and the underlying [types.Block] by number or
// hash, checking in-memory blocks first then falling back to the database.
// Returns nils for blocks that are not yet executed.
func (b *apiBackend) getReceipts(numOrHash rpc.BlockNumberOrHash) (types.Receipts, *types.Block, error) {
	numOrHash.RequireCanonical = true

	blk, err := readByNumberOrHash(
		b,
		numOrHash,
		func(b *blocks.Block) *blocks.Block {
			return b
		},
		func(db ethdb.Reader, h common.Hash, num uint64) (*blocks.Block, error) {
			if num > b.vm.LastExecuted().Height() {
				return nil, blocks.ErrNotFound
			}
			blk, err := blocks.New(rawdb.ReadBlock(db, h, num), nil, nil, b.vm.Logger())
			if err != nil {
				return nil, err
			}
			if err := blk.RestoreExecutionArtefacts(b.vm.DB(), b.vm.XDB(), b.ChainConfig()); err != nil {
				return nil, err
			}
			return blk, nil
		},
	)
	switch {
	case errors.Is(err, blocks.ErrNotFound):
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	case !blk.Executed():
		return nil, nil, nil
	default:
		return blk.Receipts(), blk.EthBlock(), nil
	}
}

type blockChainAPI struct {
	*ethapi.BlockChainAPI
	b *apiBackend
}

// We override [ethapi.BlockChainAPI.GetBlockReceipts] so that we do not return
// an error when a user queries a known, but not yet executed, block.
func (b *blockChainAPI) GetBlockReceipts(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) ([]map[string]any, error) {
	receipts, blk, err := b.b.getReceipts(blockNrOrHash)
	if err != nil || blk == nil {
		return nil, nil //nolint:nilerr // This follows Geth behavior for [ethapi.BlockChainAPI.GetBlockReceipts]
	}

	hash := blk.Hash()
	num := blk.NumberU64()
	signer := b.b.vm.SignerForBlock(blk)
	txs := blk.Transactions()

	result := make([]map[string]any, len(txs))
	for i, receipt := range receipts {
		result[i] = ethapi.MarshalReceipt(receipt, hash, num, signer, txs[i], i)
	}
	return result, nil
}

// PendingBlockAndReceipts returns a nil block and receipts. Returning nil tells
// geth that this backend does not support pending blocks. In SAE, the pending
// block is defined as the most recently accepted block, but receipts are only
// available after execution. Returning a non-nil block with incorrect or empty
// receipts could cause geth to encounter errors.
func (*apiBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return nil, nil
}

func (b *apiBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	return rawdb.ReadLogs(b.vm.DB(), blockHash, number), nil
}

type immediateReceipts struct {
	vm VM
	*ethapi.TransactionAPI
}

func (ir immediateReceipts) GetTransactionReceipt(ctx context.Context, h common.Hash) (map[string]any, error) {
	r, ok, err := ir.vm.RecentReceipt(ctx, h)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The transaction has either not been included yet, or it was cleared
		// from the [saexec.Executor] cache but is on disk. The standard
		// mechanism already differentiates between these scenarios.
		return ir.TransactionAPI.GetTransactionReceipt(ctx, h)
	}
	return ethapi.MarshalReceipt(
		r.Receipt,
		r.BlockHash,
		r.BlockNumber.Uint64(),
		r.Signer,
		r.Tx,
		int(r.TransactionIndex), //nolint:gosec // Known to not overflow
	), nil
}
