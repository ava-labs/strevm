package sae

import (
	"context"
	"math/big"

	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/accounts"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/rpc"
)

// This file provides an explicit view of the [ethAPIBackend] methods stil
// required to fully implement [ethapi.Backend] (instead of simply embedding the
// latter). It is safe for them to panic because [rpc.Server] will recover and
// report that the method handler crashed.

var _ ethapi.Backend = (*ethAPIBackend)(nil)

func (b *ethAPIBackend) SyncProgress() ethereum.SyncProgress {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) FeeHistory(ctx context.Context, blockCount uint64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) ChainDb() ethdb.Database           { panic(errUnimplemented) }
func (b *ethAPIBackend) AccountManager() *accounts.Manager { panic(errUnimplemented) }
func (b *ethAPIBackend) ExtRPCEnabled() bool               { panic(errUnimplemented) }
func (b *ethAPIBackend) SetHead(number uint64)             { panic(errUnimplemented) }

func (b *ethAPIBackend) StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetPoolTransactions() (types.Transactions, error) { panic(errUnimplemented) }

func (b *ethAPIBackend) GetPoolTransaction(txHash common.Hash) *types.Transaction {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) Stats() (pending int, queued int) { panic(errUnimplemented) }

func (b *ethAPIBackend) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) BloomStatus() (uint64, uint64) {
	panic(errUnimplemented)
}

func (b *ethAPIBackend) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	panic(errUnimplemented)
}
