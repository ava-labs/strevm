// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/network/p2p/gossip"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/strevm/txgossip"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	gossipHandlerID   = p2p.TxGossipHandlerID
	pullRequestPeriod = time.Second // seconds/request
	pushGossipPeriod  = 100 * time.Millisecond
)

// newNetwork creates the P2P network with a registered validator set.
func newNetwork(
	snowCtx *snow.Context,
	sender snowcommon.AppSender,
	reg *prometheus.Registry,
) (
	*p2p.Network,
	*p2p.Validators,
	error,
) {
	const maxValidatorSetStaleness = time.Minute
	validatorPeers := p2p.NewValidators(
		snowCtx.Log,
		snowCtx.SubnetID,
		snowCtx.ValidatorState,
		maxValidatorSetStaleness,
	)
	const p2pNamespace = "p2p"
	network, err := p2p.NewNetwork(
		snowCtx.Log,
		sender,
		reg,
		p2pNamespace,
		validatorPeers,
	)
	if err != nil {
		return nil, nil, err
	}
	return network, validatorPeers, nil
}

// newGossipers creates the tx gossipers for mempool dissemination.
func newGossipers(
	snowCtx *snow.Context,
	network *p2p.Network,
	validatorPeers *p2p.Validators,
	reg *prometheus.Registry,
	mempool *txgossip.Set,
) (
	p2p.Handler,
	*gossip.ValidatorGossiper,
	*gossip.PushGossiper[txgossip.Transaction],
	error,
) {
	const gossipNamespace = "gossip"
	metrics, err := gossip.NewMetrics(reg, gossipNamespace)
	if err != nil {
		return nil, nil, nil, err
	}

	const targetMessageSize = 20 * units.KiB
	handler := gossip.NewHandler(
		snowCtx.Log,
		txgossip.Marshaller{},
		mempool,
		metrics,
		targetMessageSize,
	)

	const (
		throttlingPeriod         = time.Hour                                     // seconds/period
		requestsPerPeerPerPeriod = float64(throttlingPeriod / pullRequestPeriod) // requests/period
	)
	throttledHandler, err := p2p.NewDynamicThrottlerHandler(
		snowCtx.Log,
		handler,
		validatorPeers,
		throttlingPeriod,
		requestsPerPeerPerPeriod,
		reg,
		gossipNamespace,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	validatorOnlyHandler := p2p.NewValidatorHandler(
		throttledHandler,
		validatorPeers,
		snowCtx.Log,
	)

	// Pull requests are filtered by validators and are throttled to prevent
	// spamming. Push messages are not filtered.
	type (
		appRequester interface {
			AppRequest(context.Context, ids.NodeID, time.Time, []byte) ([]byte, *snowcommon.AppError)
		}
		appGossiper interface {
			AppGossip(context.Context, ids.NodeID, []byte)
		}
	)
	gossipHandler := struct {
		appRequester
		appGossiper
	}{
		appRequester: validatorOnlyHandler,
		appGossiper:  handler,
	}

	client := network.NewClient(gossipHandlerID, validatorPeers)
	const pollSize = 1
	pullGossiper := gossip.NewPullGossiper[txgossip.Transaction](
		snowCtx.Log,
		txgossip.Marshaller{},
		mempool,
		client,
		metrics,
		pollSize,
	)
	pullGossiperWhenValidator := &gossip.ValidatorGossiper{
		Gossiper:   pullGossiper,
		NodeID:     snowCtx.NodeID,
		Validators: validatorPeers,
	}

	pushGossipParams := gossip.BranchingFactor{
		StakePercentage: .9,
		Validators:      100,
	}
	pushRegossipParams := gossip.BranchingFactor{
		Validators: 10,
	}
	const (
		discardedCacheSize = 16_384
		regossipPeriod     = 30 * time.Second
	)
	pushGossiper, err := gossip.NewPushGossiper(
		txgossip.Marshaller{},
		mempool,
		validatorPeers,
		client,
		metrics,
		pushGossipParams,
		pushRegossipParams,
		discardedCacheSize,
		targetMessageSize,
		regossipPeriod,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	return gossipHandler, pullGossiperWhenValidator, pushGossiper, nil
}
