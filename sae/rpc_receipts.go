// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/libevm/ethapi"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

type immediateReceipts struct {
	vm *VM
	*ethapi.TransactionAPI
}

func (ir immediateReceipts) GetTransactionReceipt(ctx context.Context, h common.Hash) (map[string]any, error) {
	var _ *saexec.Executor // protect the import for IDE comment resolution

	// [saexec.Executor.RecentReceipt] will only return false if the transaction
	// is yet to be included in an accepted block, or if it was executed so long
	// ago that it is no longer in the cache. Assuming a tx "known" to the
	// network, the former implies that it is in the mempool and the latter
	// requires a fallback to regular receipt retrieval. Since
	// [saexec.Executor.Enqueue] returning without error guarantees that
	// RecentReceipt() will return known, still-cached receipts, we only have to
	// handle the unknown ones.

	// A buffer of 1 avoids a race between our call to RecentReceipt() and a
	// concurrent call to Enqueue() by [VM.AcceptBlock], which only sends an
	// `accepted` event after enqueuing.
	ch := make(chan *blocks.Block, 1)
	sub := ir.vm.exec.SubscribeEnqueueEvent(ch)
	defer sub.Unsubscribe()

	for ; ; <-ch {
		// The mempool might have dropped the tx for reasons other than block
		// inclusion, in which case we'll fall back on regular receipt retrival,
		// which already handles unknown transactions.
		known := ir.vm.mempool.Pool.Has(h)

		r, ok, err := ir.vm.exec.RecentReceipt(ctx, h)
		if err != nil {
			return nil, err
		}
		if ok {
			return r.MarshalForRPC(), nil
		}
		if !known {
			return ir.TransactionAPI.GetTransactionReceipt(ctx, h)
		}

		// 'Cause if at first you don't succeed
		// You can dust it off and try again
		// Dust yourself off and try again, try again
		// 𝄢 ♪♩♪♩♩|♪♩♪♩♩
	}
}
