// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"context"
	"errors"

	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
)

// Add is a wrapper around [txpool.TxPool.Add], exposed to accept transactions
// over [gossip]. Populating the [Set] via this method does *not* result in new
// transactions being push-gossiped, but they will be included in pull-gossip
// responses.
func (s *Set) Add(tx Transaction) error {
	errs := s.Pool.Add([]*types.Transaction{tx.Transaction}, false, false)
	for i, err := range errs {
		if errors.Is(err, txpool.ErrAlreadyKnown) {
			errs[i] = nil
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	// DO NOT MERGE without updating to the avalanchego version that makes
	// resetting thread-safe. Also change to s.bloom.ResetIfNeeded() at the same
	// time.
	pending, queued := s.Pool.Stats()
	reset, err := gossip.ResetBloomFilterIfNeeded(s.bloom, 2*(pending+queued))
	if err != nil {
		return err
	}
	if reset {
		s.fillBloomFilter()
	}
	s.bloom.Add(tx)
	return nil
}

// SendTx implements the respective method of [ethapi.Backend], accepting
// transactions submitted via the `eth_sendTransaction` RPC method. Unlike
// [Set.Add], transactions added via this method will be push-gossiped; see
// [RegisterPushGossiper].
func (s *Set) SendTx(ctx context.Context, ethTx *types.Transaction) error {
	tx := Transaction{ethTx}
	if err := s.Add(tx); err != nil {
		return err
	}
	for _, add := range s.pushTo {
		add(tx)
	}
	return nil
}

// RegisterPushGossiper registers [gossip.PushGossiper.Add] as a callback for
// every transaction received via [Set.SendTx].
//
// NOTE: it is not safe to call this method concurrently with [Set.SendTx];
// registration is expected to occur immediately after construction of the
// [Set].
func (s *Set) RegisterPushGossiper(push *gossip.PushGossiper[Transaction]) {
	s.pushTo = append(s.pushTo, push.Add)
}
