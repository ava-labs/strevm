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
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/accounts"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/debug"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/saexec"
	"github.com/ava-labs/strevm/txgossip"
)

// APIBackend is the union of all interfaces required to implement the SAE APIs.
type APIBackend interface {
	ethapi.Backend
	filters.BloomOverrider
}

// APIBackend returns an API backend backed by the [VM].
func (vm *VM) APIBackend() APIBackend {
	return vm.apiBackend
}

func (vm *VM) ethRPCServer() (*rpc.Server, error) {
	b := vm.APIBackend()

	filterSystem := filters.NewFilterSystem(b, filters.Config{})
	filterAPI := filters.NewFilterAPI(filterSystem, false /*isLightClient*/)
	vm.toClose = append(vm.toClose, func() error {
		filters.CloseAPI(filterAPI)
		return nil
	})

	type api struct {
		namespace string
		api       any
	}

	// Standard Ethereum APIs are documented at: https://ethereum.org/developers/docs/apis/json-rpc
	// Geth-specific APIs are documented at: https://geth.ethereum.org/docs/interacting-with-geth/rpc
	apis := []api{
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
		// - eth_syncing
		{"eth", ethapi.NewEthereumAPI(b)},
		// Standard Ethereum node APIs:
		// - eth_blockNumber
		// - eth_chainId
		// - eth_getBlockByHash
		// - eth_getBlockByNumber
		// - eth_getUncleByBlockHashAndIndex
		// - eth_getUncleByBlockNumberAndIndex
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
		// - eth_sendRawTransaction
		// - eth_sendTransaction
		// - eth_sign
		// - eth_signTransaction
		//
		// Undocumented APIs:
		// - eth_getRawTransactionByBlockHashAndIndex
		// - eth_getRawTransactionByBlockNumberAndIndex
		// - eth_getRawTransactionByHash
		// - eth_pendingTransactions
		{"eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))},
		// Standard Ethereum node APIS:
		// - eth_getLogs
		//
		// Geth-specific APIs:
		// - eth_subscribe
		//  - newHeads
		//  - newPendingTransactions
		//  - logs
		{"eth", filterAPI},
	}

	if vm.config.RPCConfig.EnableProfiling {
		apis = append(apis, api{
			// Geth-specific APIs:
			// - debug_blockProfile
			// - debug_cpuProfile
			// - debug_freeOSMemory
			// - debug_gcStats
			// - debug_goTrace
			// - debug_memStats
			// - debug_mutexProfile
			// - debug_setBlockProfileRate
			// - debug_setGCPercent
			// - debug_setMutexProfileFraction
			// - debug_stacks
			// - debug_startCPUProfile
			// - debug_startGoTrace
			// - debug_stopCPUProfile
			// - debug_stopGoTrace
			// - debug_verbosity
			// - debug_vmodule
			// - debug_writeBlockProfile
			// - debug_writeMemProfile
			// - debug_writeMutexProfile
			"debug", debug.Handler,
		})
	}

	s := rpc.NewServer()
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

// chainIndexer implements the subset of [ethapi.Backend] required to back a
// [core.ChainIndexer].
type chainIndexer struct {
	exec *saexec.Executor
}

var _ core.ChainIndexerChain = chainIndexer{}

func (c chainIndexer) CurrentHeader() *types.Header {
	return types.CopyHeader(c.exec.LastExecuted().Header())
}

func (c chainIndexer) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return c.exec.SubscribeChainHeadEvent(ch)
}

// A bloomOverrider constructs Bloom filters from persisted receipts instead of
// relying on the [types.Header] field.
type bloomOverrider struct {
	db ethdb.Database
}

var _ filters.BloomOverrider = bloomOverrider{}

// OverrideHeaderBloom returns the Bloom filter of the receipts generated when
// executing the respective block, whereas the [types.Header] carries those
// settled by the block.
func (b bloomOverrider) OverrideHeaderBloom(header *types.Header) types.Bloom {
	return types.CreateBloom(rawdb.ReadRawReceipts(
		b.db,
		header.Hash(),
		header.Number.Uint64(),
	))
}

type ethAPIBackend struct {
	vm             *VM
	accountManager *accounts.Manager

	*txgossip.Set
	chainIndexer
	bloomOverrider
	*bloomIndexer

	ethapi.Backend // TODO(arr4n) remove once all methods are implemented
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

func (b *ethAPIBackend) AccountManager() *accounts.Manager {
	return b.accountManager
}

func (b *ethAPIBackend) CurrentBlock() *types.Header {
	return b.CurrentHeader()
}

func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int {
	return big.NewInt(0) // TODO(arr4n)
}

func (b *ethAPIBackend) SyncProgress() ethereum.SyncProgress {
	// Avalanchego does not expose APIs until after the node has fully synced.
	return ethereum.SyncProgress{}
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

func (b *ethAPIBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	if hash == (common.Hash{}) {
		return nil, errors.New("empty block hash")
	}
	n, err := b.ResolveBlockNumber(number)
	if err != nil {
		return nil, err
	}

	if block, ok := b.vm.blocks.Load(hash); ok {
		if block.NumberU64() != n {
			return nil, fmt.Errorf("found block number %d for hash %#x, expected %d", block.NumberU64(), hash, number)
		}
		return block.EthBlock().Body(), nil
	}

	return rawdb.ReadBody(b.vm.db, hash, n), nil
}

func (b *ethAPIBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	return rawdb.ReadLogs(b.vm.db, blockHash, number), nil
}

func (b *ethAPIBackend) GetPoolTransactions() (types.Transactions, error) {
	pending := b.Pool.Pending(txpool.PendingFilter{})

	var pendingCount int
	for _, batch := range pending {
		pendingCount += len(batch)
	}

	txs := make(types.Transactions, 0, pendingCount)
	for _, batch := range pending {
		for _, lazy := range batch {
			if tx := lazy.Resolve(); tx != nil {
				txs = append(txs, tx)
			}
		}
	}
	return txs, nil
}

type canonicalReader[T any] func(ethdb.Reader, common.Hash, uint64) *T

func readByNumber[T any](b *ethAPIBackend, n rpc.BlockNumber, read canonicalReader[T]) (*T, error) {
	num, err := b.ResolveBlockNumber(n)
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

func (b *ethAPIBackend) ResolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
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

func (b *ethAPIBackend) SubscribeChainSideEvent(chan<- core.ChainSideEvent) event.Subscription {
	// SAE never reorgs, so there are no side events.
	return newNoopSubscription()
}

func (b *ethAPIBackend) SubscribeChainAcceptedEvent(ch chan<- *types.Block) event.Subscription {
	return b.vm.exec.SubscribeBlockEnqueueEvent(ch)
}

func (b *ethAPIBackend) NextBaseFeeUpperBound(context.Context) *big.Int {
	bounds := b.vm.last.accepted.Load().WorstCaseBounds()
	if bounds == nil || bounds.NextGasTime == nil {
		return nil
	}
	return bounds.NextGasTime.BaseFee().ToBig()
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
