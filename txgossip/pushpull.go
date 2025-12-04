// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"context"

	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/ethapi"
)

// SendTx implements the respective method of [ethapi.Backend], accepting
// transactions submitted via the `eth_sendTransaction` RPC method. Unlike
// [gossip.BloomSet.Add], transactions added via this method will be
// push-gossiped; see [Set.RegisterPushGossiper].
func (s *Set) SendTx(ctx context.Context, ethTx *types.Transaction) error {
	var _ ethapi.Backend // protect the import for [comment] rendering

	// We filter out [txpool.ErrAlreadyKnown] in [txSet.Add] but want to keep it
	// for RPCs.
	if s.set.pool.Has(ethTx.Hash()) {
		return txpool.ErrAlreadyKnown
	}

	tx := Transaction{ethTx}
	if err := s.BloomSet.Add(tx); err != nil {
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
