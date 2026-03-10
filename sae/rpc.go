// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	saerpc "github.com/ava-labs/strevm/sae/rpc"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/txgossip"
)

type APIBackend = saerpc.Backend // DO NOT MERGE

// APIBackend returns an API backend backed by the [VM].
func (vm *VM) APIBackend() saerpc.Backend {
	return vm.rpcAPIs.Backend()
}

func (vm *VM) ethRPCServer() (*rpc.Server, error) {
	return vm.rpcAPIs.NewServer()
}

var _ saerpc.VM = rpcSource{}

type rpcSource struct {
	*VM
	*saexec.Executor
}

func (s rpcSource) Logger() logging.Logger                              { return s.VM.snowCtx.Log }
func (s rpcSource) Hooks() hook.Points                                  { return s.hooks }
func (s rpcSource) DB() ethdb.Database                                  { return s.db }
func (s rpcSource) XDB() saedb.ExecutionResults                         { return s.xdb }
func (s rpcSource) Mempool() *txgossip.Set                              { return s.mempool }
func (s rpcSource) Peers() *p2p.Peers                                   { return s.peers }
func (s rpcSource) BlockFromMemory(h common.Hash) (*blocks.Block, bool) { return s.blocks.Load(h) }
func (s rpcSource) LastAccepted() *blocks.Block                         { return s.last.accepted.Load() }
func (s rpcSource) LastSettled() *blocks.Block                          { return s.last.settled.Load() }

func (s rpcSource) NewBlock(eth *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error) {
	return s.blockBuilder.new(eth, parent, lastSettled)
}

func (s rpcSource) SettledBlockFromDB(db ethdb.Reader, hash common.Hash, num uint64) (*blocks.Block, error) {
	return s.settledBlockFromDB(db, hash, num)
}

func (s rpcSource) SubscribeAcceptedBlocks(ch chan<- *blocks.Block) event.Subscription {
	return s.acceptedBlocks.Subscribe(ch)
}

// ResolveBlockNumber resolves the [rpc.BlockNumber], supporting the following
// named blocks:
//
// - [rpc.PendingBlockNumber]: last accepted (i.e. pending execution)
// - [rpc.LatestBlockNumber]: last executed
// - [rpc.SafeBlockNumber] and [rpc.FinalizedBlockNumber]: last settled
//
// Explicit, positive block numbers are returned unmodified as long as the block
// has been accepted.
//
// NOTE: the definition of safe and finalized as the last-settled block DOES NOT
// affect finality of consensus under SAE, which is immediate upon acceptance.
// Safe blocks can be thought of as safe against hard-drive corruption on the
// specific validator, while final blocks are labelled as such only to maintain
// monotonicity of the naming convention.
func (s rpcSource) ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := s.LastAccepted().Height()

	switch bn {
	case rpc.PendingBlockNumber:
		return head, nil
	case rpc.LatestBlockNumber:
		return s.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return s.LastSettled().Height(), nil
	}

	if bn < 0 {
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > head {
		return 0, fmt.Errorf("%w: block %d", saerpc.ErrFutureBlockNotResolved, n)
	}
	return n, nil
}
