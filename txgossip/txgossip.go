// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package txgossip provides a mempool for [Streaming Asynchronous Execution],
// which is also compatible with AvalancheGo's [gossip] mechanism.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package txgossip

import (
	"errors"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"
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

// A Set is a [gossip.Set] wrapping a [txpool.TxPool]. Transactions MAY be added
// to the pool directly, or via [Set.Add].
type Set struct {
	Pool  *txpool.TxPool
	bloom *gossip.BloomFilter

	txSub     event.Subscription
	bloomDone chan error
}

// NewSet returns a new [gossip.Set]. [Set.Close] MUST be called to release
// resources.
func NewSet(logger logging.Logger, pool *txpool.TxPool, bloom *gossip.BloomFilter) *Set {
	fillBloomFilter(pool, bloom)

	txs := make(chan core.NewTxsEvent)
	s := &Set{
		Pool:      pool,
		bloom:     bloom,
		txSub:     pool.SubscribeTransactions(txs, false),
		bloomDone: make(chan error, 1),
	}
	go s.maintainBloomFilter(logger, txs)

	return s
}

func fillBloomFilter(pool *txpool.TxPool, bloom *gossip.BloomFilter) {
	pending, queued := pool.Content()
	for _, txSet := range []map[common.Address][]*types.Transaction{pending, queued} {
		for _, txs := range txSet {
			for _, tx := range txs {
				bloom.Add(Transaction{tx})
			}
		}
	}
}

func (s *Set) maintainBloomFilter(logger logging.Logger, txs <-chan core.NewTxsEvent) {
	for {
		select {
		case ev := <-txs:
			pending, queued := s.Pool.Stats()
			if _, err := gossip.ResetBloomFilterIfNeeded(s.bloom, 2*(pending+queued)); err != nil {
				logger.Error("Resetting mempool bloom filter", zap.Error(err))
			}
			for _, tx := range ev.Txs {
				s.bloom.Add(Transaction{tx})
			}

		case err, ok := <-s.txSub.Err():
			// [event.Subscription] documents semantics of its error channel,
			// stating that at most one error will ever be sent, and that it
			// will be closed when unsubscribing.
			if ok {
				logger.Error("TxPool subscription", zap.Error(err))
				s.bloomDone <- err
			} else {
				close(s.bloomDone)
			}
			return
		}
	}
}

// Close stops background work being performed by the [Set], and returns the
// last error encountered by said processes.
func (s *Set) Close() error {
	s.txSub.Unsubscribe()
	return <-s.bloomDone
}

// Add is a wrapper around [txpool.TxPool.Add], exposed to accept transactions
// over [gossip]. It MAY be bypassed, and the pool's method accessed directly.
func (s *Set) Add(tx Transaction) error {
	errs := s.Pool.Add([]*types.Transaction{tx.Transaction}, false, false)
	return errors.Join(errs...)
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
