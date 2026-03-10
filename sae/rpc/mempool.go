package rpc

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
)

func (b *apiBackend) Stats() (pending int, queued int) {
	return b.Set.Pool.Stats()
}

func (b *apiBackend) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	return b.Set.Pool.Content()
}

func (b *apiBackend) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	return b.Set.Pool.ContentFrom(addr)
}

func (b *apiBackend) GetPoolTransactions() (types.Transactions, error) {
	pending := b.Pool.Pending(txpool.PendingFilter{})

	var pendingCount int
	for _, batch := range pending {
		pendingCount += len(batch)
	}

	txs := make(types.Transactions, 0, pendingCount)
	for _, batch := range pending {
		for _, lazy := range batch {
			if tx := lazy.Resolve(); tx != nil {
				txs = append(txs, tx)
			}
		}
	}
	return txs, nil
}
