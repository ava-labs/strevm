// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/txgossip"
)

func (vm *VM) ethRPCServer(config rpcConfig) (*rpc.Server, error) {
	b := newEthAPIBackend(config, vm)
	s := rpc.NewServer()

	// Even if this function errors, we should close API to prevent a goroutine
	// from leaking.
	filterSystem := filters.NewFilterSystem(b, filters.Config{})
	filterAPI := filters.NewFilterAPI(filterSystem, false /*isLightClient*/)
	vm.toClose = append(vm.toClose, func() error {
		filters.CloseAPI(filterAPI)
		return b.bloomIndexer.close()
	})

	// Standard Ethereum APIs are documented at: https://ethereum.org/developers/docs/apis/json-rpc
	// Geth-specific APIs are documented at: https://geth.ethereum.org/docs/interacting-with-geth/rpc
	apis := []struct {
		namespace string
		api       any
	}{
		// Standard Ethereum node APIs:
		// - web3_clientVersion
		// - web3_sha3
		{"web3", newWeb3API()},
		// Standard Ethereum node APIs:
		// - net_listening
		// - net_peerCount
		// - net_version
		{"net", newNetAPI(vm.peers, vm.exec.ChainConfig().ChainID.Uint64())},
		// Geth-specific APIs:
		// - txpool_content
		// - txpool_contentFrom
		// - txpool_inspect
		// - txpool_status
		{"txpool", ethapi.NewTxPoolAPI(b)},

		// Standard Ethereum node APIs:
		// - eth_blockNumber
		// - eth_getBlockByHash
		// - eth_getBlockByNumber
		// - eth_getUncleCountByBlockHash
		// - eth_getUncleCountByBlockNumber
		//
		// Geth-specific APIs:
		// - eth_getHeaderByHash
		// - eth_getHeaderByNumber
		{"eth", ethapi.NewBlockChainAPI(b)},
		// Standard Ethereum node APIs:
		// - eth_getBlockTransactionCountByHash
		// - eth_getBlockTransactionCountByNumber
		// - eth_getTransactionByBlockHashAndIndex
		// - eth_getTransactionByBlockNumberAndIndex
		// - eth_getTransactionByHash
		//
		// Undocumented APIs:
		// - eth_getRawTransactionByHash
		// - eth_getRawTransactionByBlockHashAndIndex
		// - eth_getRawTransactionByBlockNumberAndIndex
		{"eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))},
		{"eth", filterAPI},
	}
	for _, api := range apis {
		if err := s.RegisterName(api.namespace, api.api); err != nil {
			return nil, fmt.Errorf("%T.RegisterName(%q, %T): %v", s, api.namespace, api.api, err)
		}
	}
	return s, nil
}

// web3API offers the `web3` RPCs.
type web3API struct {
	clientVersion string
}

func newWeb3API() *web3API {
	return &web3API{
		clientVersion: version.GetVersions().String(),
	}
}

func (w *web3API) ClientVersion() string {
	return w.clientVersion
}

func (*web3API) Sha3(input hexutil.Bytes) hexutil.Bytes {
	return crypto.Keccak256(input)
}

// netAPI offers the `net` RPCs.
type netAPI struct {
	peers   *p2p.Peers
	chainID string
}

func newNetAPI(peers *p2p.Peers, chainID uint64) *netAPI {
	return &netAPI{
		peers:   peers,
		chainID: strconv.FormatUint(chainID, 10),
	}
}

func (s *netAPI) Listening() bool {
	return true // The node is always listening for p2p connections.
}

func (s *netAPI) PeerCount() hexutil.Uint {
	c := s.peers.Len()
	if c <= 0 {
		return 0
	}
	// Peers includes ourself, so we subtract one.
	return hexutil.Uint(c) - 1
}

func (s *netAPI) Version() string {
	return s.chainID
}

type rpcConfig struct {
	bloomSectionSize uint64 // Number of blocks per bloom section
}

var (
	_ core.ChainIndexerChain  = (*ethAPIBackend)(nil)
	_ filters.BloomFromHeader = (*ethAPIBackend)(nil)
)

type ethAPIBackend struct {
	ethapi.Backend // TODO(arr4n) remove in favour of `var _ ethapi.Backend = (*ethAPIBackend)(nil)`
	*txgossip.Set

	vm           *VM
	bloomIndexer *bloomIndexer
}

func newEthAPIBackend(config rpcConfig, vm *VM) *ethAPIBackend {
	b := &ethAPIBackend{
		Set:          vm.mempool,
		vm:           vm,
		bloomIndexer: newBloomIndexer(vm.db, config.bloomSectionSize),
	}

	// TODO: if we are state syncing, we need to provide the first block available to the indexer
	// via [core.ChainIndexer.AddCheckpoint].
	b.bloomIndexer.start(b)

	return b
}

func (b *ethAPIBackend) BloomStatus() (uint64, uint64) {
	return b.bloomIndexer.status()
}

func (b *ethAPIBackend) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	b.bloomIndexer.spawnFilter(ctx, session)
}

// HeaderBloom retrieves the bloom filter for a given header.
// The bloom actually stored in the header is the bloom of the receipts that the block settled, not the
// bloom of the transactions in the block, so we must retrieve the blocks receipts from the database.
// The [*filters.FilterAPI] relies on this method, although not in [ethapi.Backend] directly.
// TODO: should we cache this? Store blooms separately?
func (b *ethAPIBackend) HeaderBloom(header *types.Header) types.Bloom {
	receipts := rawdb.ReadRawReceipts(b.ChainDb(), header.Hash(), header.Number.Uint64())
	return types.CreateBloom(receipts)
}

func (b *ethAPIBackend) ChainDb() ethdb.Database {
	return b.vm.db
}

func (b *ethAPIBackend) ChainConfig() *params.ChainConfig {
	return b.vm.exec.ChainConfig()
}

func (b *ethAPIBackend) RPCTxFeeCap() float64 {
	return 0 // TODO(arr4n)
}

func (b *ethAPIBackend) UnprotectedAllowed() bool {
	return false
}

func (b *ethAPIBackend) CurrentHeader() *types.Header {
	return types.CopyHeader(b.vm.exec.LastExecuted().Header())
}

func (b *ethAPIBackend) CurrentBlock() *types.Header {
	return b.CurrentHeader()
}

func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int {
	return big.NewInt(0) // TODO(arr4n)
}

func (b *ethAPIBackend) HeaderByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Header, error) {
	return readByNumber(b, n, rawdb.ReadHeader)
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	return readByNumber(b, n, rawdb.ReadBlock)
}

func (b *ethAPIBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	if b, ok := b.vm.blocks.Load(hash); ok {
		return b.Header(), nil
	}
	return readByHash(b, hash, rawdb.ReadHeader), nil
}

func (b *ethAPIBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	if b, ok := b.vm.blocks.Load(hash); ok {
		return b.EthBlock(), nil
	}
	return readByHash(b, hash, rawdb.ReadBlock), nil
}

func (b *ethAPIBackend) GetTransaction(ctx context.Context, txHash common.Hash) (exists bool, tx *types.Transaction, blockHash common.Hash, blockNumber uint64, index uint64, err error) {
	tx, blockHash, blockNumber, index = rawdb.ReadTransaction(b.vm.db, txHash)
	if tx == nil {
		return false, nil, common.Hash{}, 0, 0, nil
	}
	return true, tx, blockHash, blockNumber, index, nil
}

func (b *ethAPIBackend) GetPoolTransaction(txHash common.Hash) *types.Transaction {
	return b.Set.Pool.Get(txHash)
}

var errInvalidArguments = errors.New("invalid arguments")

func (b *ethAPIBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	if number < 0 || hash == (common.Hash{}) {
		return nil, errInvalidArguments
	}
	if block, ok := b.vm.blocks.Load(hash); ok {
		return block.EthBlock().Body(), nil
	}
	return rawdb.ReadBody(b.vm.db, hash, uint64(number)), nil //nolint:gosec // Non-negative check performed above
}

func (b *ethAPIBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	return rawdb.ReadLogs(b.vm.db, blockHash, number), nil
}

type canonicalReader[T any] func(ethdb.Reader, common.Hash, uint64) *T

func readByNumber[T any](b *ethAPIBackend, n rpc.BlockNumber, read canonicalReader[T]) (*T, error) {
	num, err := b.resolveBlockNumber(n)
	if errors.Is(err, errFutureBlockNotResolved) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return read(b.vm.db, rawdb.ReadCanonicalHash(b.vm.db, num), num), nil
}

func readByHash[T any](b *ethAPIBackend, hash common.Hash, read canonicalReader[T]) *T {
	num := rawdb.ReadHeaderNumber(b.vm.db, hash)
	if num == nil {
		return nil
	}
	return read(b.vm.db, hash, *num)
}

var errFutureBlockNotResolved = errors.New("not accepted yet")

func (b *ethAPIBackend) resolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := b.vm.last.accepted.Load().Height()

	switch bn {
	case rpc.PendingBlockNumber: // i.e. pending execution
		return head, nil
	case rpc.LatestBlockNumber:
		return b.vm.exec.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return b.vm.last.settled.Load().Height(), nil
	}

	if bn < 0 {
		// Any future definitions should be added above.
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > head {
		return 0, fmt.Errorf("%w: block %d", errFutureBlockNotResolved, n)
	}
	return n, nil
}

func (b *ethAPIBackend) Stats() (pending int, queued int) {
	return b.Set.Pool.Stats()
}

func (b *ethAPIBackend) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	return b.Set.Pool.Content()
}

func (b *ethAPIBackend) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	return b.Set.Pool.ContentFrom(addr)
}

func (b *ethAPIBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return b.vm.exec.SubscribeChainEvent(ch)
}

func (b *ethAPIBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return b.vm.exec.SubscribeChainHeadEvent(ch)
}

func (b *ethAPIBackend) SubscribeChainSideEvent(chan<- core.ChainSideEvent) event.Subscription {
	// SAE never reorgs, so there are no side events.
	return newNoopSubscription()
}

func (b *ethAPIBackend) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return b.Set.Pool.SubscribeTransactions(ch, true)
}

func (b *ethAPIBackend) SubscribeRemovedLogsEvent(chan<- core.RemovedLogsEvent) event.Subscription {
	// SAE never reorgs, so no logs are ever removed.
	return newNoopSubscription()
}

func (b *ethAPIBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return b.vm.exec.SubscribeLogsEvent(ch)
}

func (b *ethAPIBackend) SubscribePendingLogsEvent(chan<- []*types.Log) event.Subscription {
	// In SAE, "pending" refers to the execution status. There are no logs known
	// for transactions pending execution.
	return newNoopSubscription()
}

type noopSubscription struct {
	once sync.Once
	err  chan error
}

func newNoopSubscription() *noopSubscription {
	return &noopSubscription{
		err: make(chan error),
	}
}

func (s *noopSubscription) Err() <-chan error {
	return s.err
}

func (s *noopSubscription) Unsubscribe() {
	s.once.Do(func() {
		close(s.err)
	})
}
