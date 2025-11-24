// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package txgossip provides a mempool for [Streaming Asynchronous Execution],
// which is also compatible with AvalancheGo's [gossip] mechanism.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package txgossip

import (
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
)

var (
	_ gossip.Gossipable              = Transaction{}
	_ gossip.Marshaller[Transaction] = Marshaller{}
	_ gossip.Set[Transaction]        = (*Set)(nil)
)

// A Transaction is a [gossip.Gossipable] wrapper for a [types.Transaction].
type Transaction struct {
	*types.Transaction
}

// GossipID returns the transaction hash.
func (tx Transaction) GossipID() ids.ID {
	return ids.ID(tx.Hash())
}

// A Marshaller implements [gossip.Marshaller] for [Transaction], based on RLP
// encoding.
type Marshaller struct{}

// MarshalGossip returns the [rlp] encoding of the underlying
// [types.Transaction].
func (Marshaller) MarshalGossip(tx Transaction) ([]byte, error) {
	return rlp.EncodeToBytes(tx.Transaction)
}

// UnmarshalGossip [rlp] decodes the buffer into a [types.Transaction].
func (Marshaller) UnmarshalGossip(buf []byte) (Transaction, error) {
	tx := Transaction{new(types.Transaction)}
	if err := rlp.DecodeBytes(buf, tx.Transaction); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// A Set is a [gossip.Set] wrapping a [txpool.TxPool].
type Set struct {
	Pool   *txpool.TxPool
	bloom  *gossip.BloomFilter
	pushTo []func(...Transaction)
}

// NewSet returns a new [gossip.Set]. See [Set.Add] and [Set.SendTx] for ways to
// add transactions to the pool, which SHOULD NOT be populated directly.
func NewSet(logger logging.Logger, pool *txpool.TxPool, bloom *gossip.BloomFilter) *Set {
	s := &Set{
		Pool:  pool,
		bloom: bloom,
	}
	s.fillBloomFilter()
	return s
}

func (s *Set) fillBloomFilter() {
	pending, queued := s.Pool.Content()
	for _, txSet := range []map[common.Address][]*types.Transaction{pending, queued} {
		for _, txs := range txSet {
			for _, tx := range txs {
				s.bloom.Add(Transaction{tx})
			}
		}
	}
}

// Has returns [txpool.TxPool.Has].
func (s *Set) Has(id ids.ID) bool {
	return s.Pool.Has(common.Hash(id))
}

// Iterate calls `fn` for every pending and queued transaction returned by
// [txpool.TxPool.Content]
func (s *Set) Iterate(fn func(Transaction) bool) {
	pending, queued := s.Pool.Content()
	for _, group := range []map[common.Address][]*types.Transaction{pending, queued} {
		for _, txs := range group {
			for _, tx := range txs {
				if !fn(Transaction{tx}) {
					return
				}
			}
		}
	}
}

// GetFilter returns [gossip.BloomFilter.Marshal] for a Bloom filter of the
// transactions in the pool.
func (s *Set) GetFilter() ([]byte, []byte) {
	return s.bloom.Marshal()
}
