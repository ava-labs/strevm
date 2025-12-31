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
	pullRequestPeriod = time.Second // seconds/request
	pushGossipPeriod  = 100 * time.Millisecond
)

func newNetwork(
	snowCtx *snow.Context,
	sender snowcommon.AppSender,
	reg *prometheus.Registry,
	mempool *txgossip.Set,
) (
	*p2p.Network,
	*gossip.ValidatorGossiper,
	*gossip.PushGossiper[txgossip.Transaction],
	error,
) {
	const maxValidatorSetStaleness = time.Minute
	var (
		peers          p2p.Peers
		validatorPeers = p2p.NewValidators(
			snowCtx.Log,
			snowCtx.SubnetID,
			snowCtx.ValidatorState,
			maxValidatorSetStaleness,
		)
	)
	const p2pNamespace = "p2p"
	network, err := p2p.NewNetwork(
		snowCtx.Log,
		sender,
		reg,
		p2pNamespace,
		&peers,
		validatorPeers,
	)
	if err != nil {
		return nil, nil, nil, err
	}

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
	if err := network.AddHandler(p2p.TxGossipHandlerID, gossipHandler); err != nil {
		return nil, nil, nil, err
	}

	client := network.NewClient(p2p.TxGossipHandlerID, validatorPeers)
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
	return network, pullGossiperWhenValidator, pushGossiper, nil
}
