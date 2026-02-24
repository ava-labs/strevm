// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/libevm/ethapi"

	"github.com/ava-labs/strevm/saexec"
)

type immediateReceipts struct {
	exec *saexec.Executor
	*ethapi.TransactionAPI
}

func (ir immediateReceipts) GetTransactionReceipt(ctx context.Context, h common.Hash) (map[string]any, error) {
	r, ok, err := ir.exec.RecentReceipt(ctx, h)
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
