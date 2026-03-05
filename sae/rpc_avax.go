// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

// Avalanche-specific "avax" namespace RPCs for atomic transactions. These are
// served on a separate HTTP endpoint (/avax) using gorilla/rpc, matching the
// pattern established in graft/coreth/plugin/evm/atomic/vm.
//
// The actual implementations will live in coreth but strevm provides the
// infrastructure to support the endpoint.
//
// Reference implementation: graft/coreth/plugin/evm/atomic/vm/api.go

import (
	"net/http"

	"github.com/ava-labs/avalanchego/api"
	avajson "github.com/ava-labs/avalanchego/utils/json"
	gorillarpc "github.com/gorilla/rpc/v2"
)

const avaxEndpoint = "/avax"

// avaxAPI offers Avalanche network related API methods for atomic transactions:
//   - avax.getAtomicTx
//   - avax.issueTx
//   - avax.getUTXOs
type avaxAPI struct{}

func newAvaxHandler() (http.Handler, error) {
	server := gorillarpc.NewServer()
	codec := avajson.NewCodec()
	server.RegisterCodec(codec, "application/json")
	server.RegisterCodec(codec, "application/json;charset=UTF-8")
	return server, server.RegisterService(&avaxAPI{}, "avax")
}

// FormattedTx is the response for GetAtomicTx.
type FormattedTx struct {
	api.FormattedTx
	BlockHeight *avajson.Uint64 `json:"blockHeight,omitempty"`
}

// GetUTXOs gets all utxos for passed in addresses.
func (*avaxAPI) GetUTXOs(r *http.Request, args *api.GetUTXOsArgs, reply *api.GetUTXOsReply) error {
	panic(errUnimplemented)
}

// IssueTx issues an atomic transaction.
func (*avaxAPI) IssueTx(r *http.Request, args *api.FormattedTx, reply *api.JSONTxID) error {
	panic(errUnimplemented)
}

// GetAtomicTx returns the specified atomic transaction.
func (*avaxAPI) GetAtomicTx(r *http.Request, args *api.GetTxArgs, reply *FormattedTx) error {
	panic(errUnimplemented)
}
