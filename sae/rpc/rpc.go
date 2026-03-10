// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/accounts"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/eth/tracers"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gasprice"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saedb"
	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/txgossip"
)

var ErrFutureBlockNotResolved = errors.New("not accepted yet")

type VM interface {
	Logger() logging.Logger
	Hooks() hook.Points

	DB() ethdb.Database
	XDB() saedb.ExecutionResults
	StateDB(common.Hash) (*state.StateDB, error)

	Mempool() *txgossip.Set
	Peers() *p2p.Peers

	ChainConfig() *params.ChainConfig
	ChainContext() core.ChainContext

	NewBlock(eth *types.Block, parent, lastSettled *blocks.Block) (*blocks.Block, error)
	BlockFromMemory(common.Hash) (*blocks.Block, bool)
	SettledBlockFromDB(db ethdb.Reader, hash common.Hash, num uint64) (*blocks.Block, error)

	SignerForBlock(*types.Block) types.Signer

	LastAccepted() *blocks.Block
	LastExecuted() *blocks.Block
	LastSettled() *blocks.Block
	ResolveBlockNumber(rpc.BlockNumber) (uint64, error)
	RecentReceipt(context.Context, common.Hash) (*saexec.Receipt, bool, error)

	SubscribeAcceptedBlocks(chan<- *blocks.Block) event.Subscription
	SubscribeChainEvent(chan<- core.ChainEvent) event.Subscription
	SubscribeChainHeadEvent(chan<- core.ChainHeadEvent) event.Subscription
	SubscribeLogsEvent(chan<- []*types.Log) event.Subscription
}

// Config provides options for initialization of RPCs for the node.
type Config struct {
	BlocksPerBloomSection uint64
	EnableDBInspecting    bool
	EnableProfiling       bool
	DisableTracing        bool
	EVMTimeout            time.Duration
	GasCap                uint64
	TxFeeCap              float64 // 0 = no cap
}

// Backend is the union of all interfaces required to implement the Ethereum
// APIs.
type Backend interface {
	ethapi.Backend
	tracers.Backend
	filters.BloomOverrider
	filters.Backend
}

type APIs struct {
	vm VM

	backend  *apiBackend
	Filter   *filters.FilterAPI
	Accounts *accounts.Manager
	GasPrice *gasprice.Estimator

	bloomIdx *bloomIndexer
}

var _ io.Closer = (*APIs)(nil)

func (a *APIs) Close() error {
	filters.CloseAPI(a.Filter)
	return errors.Join(
		a.Accounts.Close(),
		a.bloomIdx.Close(),
		a.GasPrice.Close(),
	)
}

func (a *APIs) Backend() Backend {
	return a.backend
}

func New(vm VM, config Config) (*APIs, error) {
	gaspricer, err := gasprice.NewEstimator(&estimatorBackend{vm}, vm.Logger(), gasprice.DefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("gasprice.NewEstimator(...): %v", err)
	}

	chainIdx := chainIndexer{vm}
	override := bloomOverrider{vm.DB()}

	back := &apiBackend{
		config, vm,
		// An empty account manager provides graceful errors for signing RPCs
		// (e.g. eth_sign) instead of nil-pointer panics. No actual account
		// functionality is expected.
		accounts.NewManager(&accounts.Config{}),
		gaspricer,
		vm.Mempool(),
		chainIdx,
		override,
		newBloomIndexer(
			// TODO(alarso16): if we are state syncing, we need to provide the
			// first block available to the indexer via
			// [core.ChainIndexer.AddCheckpoint].
			vm.DB(),
			chainIdx,
			override,
			config.BlocksPerBloomSection,
		),
	}

	return &APIs{
		vm,
		back,
		filters.NewFilterAPI(
			filters.NewFilterSystem(back, filters.Config{}),
			false, /*isLightClient*/
		),
		back.accountManager,
		back.Estimator,
		back.bloomIndexer,
	}, nil
}

var _ Backend = (*apiBackend)(nil)

type apiBackend struct {
	config         Config
	vm             VM
	accountManager *accounts.Manager

	*gasprice.Estimator
	*txgossip.Set
	chainIndexer
	bloomOverrider
	*bloomIndexer
}

var (
	ErrNeitherNumberNorHash = fmt.Errorf("%T carrying neither number nor hash", rpc.BlockNumberOrHash{})
	ErrBothNumberAndHash    = fmt.Errorf("%T carrying both number and hash", rpc.BlockNumberOrHash{})
	ErrNonCanonicalBlock    = errors.New("non-canonical block")
)

func (b *apiBackend) resolveBlockNumberOrHash(numOrHash rpc.BlockNumberOrHash) (uint64, common.Hash, error) {
	rpcNum, isNum := numOrHash.Number()
	hash, isHash := numOrHash.Hash()

	switch {
	case isNum && isHash:
		return 0, common.Hash{}, ErrBothNumberAndHash

	case isNum:
		num, err := b.vm.ResolveBlockNumber(rpcNum)
		if err != nil {
			return 0, common.Hash{}, err
		}

		hash := rawdb.ReadCanonicalHash(b.vm.DB(), num)
		if hash == (common.Hash{}) {
			return 0, common.Hash{}, fmt.Errorf("block %d not found", num)
		}
		return num, hash, nil

	case isHash:
		if bl, ok := b.vm.BlockFromMemory(hash); ok {
			n := bl.NumberU64()
			if numOrHash.RequireCanonical && hash != rawdb.ReadCanonicalHash(b.vm.DB(), n) {
				return 0, common.Hash{}, ErrNonCanonicalBlock
			}
			return n, hash, nil
		}

		numPtr := rawdb.ReadHeaderNumber(b.vm.DB(), hash)
		if numPtr == nil {
			return 0, common.Hash{}, fmt.Errorf("block %#x not found", hash)
		}
		// We only write canonical blocks to the database so there's no need to
		// perform a check.
		return *numPtr, hash, nil

	default:
		return 0, common.Hash{}, ErrNeitherNumberNorHash
	}
}
