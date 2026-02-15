// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/libevm/ethapi"

	"github.com/ava-labs/strevm/cache"
	"github.com/ava-labs/strevm/saexec"
)

type immediateReceipts struct {
	recent *cache.UniformlyKeyed[common.Hash, *saexec.ReceiptForRPC]
	*ethapi.TransactionAPI
}

func (ir immediateReceipts) GetTransactionReceipt(ctx context.Context, h common.Hash) (map[string]any, error) {
	if r, ok := ir.recent.Load(h); ok {
		return ethapi.MarshalReceipt(r.Receipt, r.BlockHash, r.BlockNumber, r.Signer, r.Tx, r.TxIndex), nil
	}
	return ir.TransactionAPI.GetTransactionReceipt(ctx, h)
}
