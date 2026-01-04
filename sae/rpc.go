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
	"time"

	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/version"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/accounts"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/bloombits"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/eth/tracers"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/debug"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/txgossip"
)

func (vm *VM) ethRPCServer() (*rpc.Server, error) {
	accountManager := accounts.NewManager(&accounts.Config{})
	vm.toClose = append(vm.toClose, accountManager.Close)

	b := &apiBackend{
		Set:            vm.mempool,
		vm:             vm,
		accountManager: accountManager,
	}

	filterSystem := filters.NewFilterSystem(b, filters.Config{})
	filterAPI := filters.NewFilterAPI(filterSystem, false /*isLightClient*/)
	vm.toClose = append(vm.toClose, func() error {
		filterAPI.Close()
		return nil
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
		// Standard Ethereum node APIs:
		// - eth_gasPrice
		// - eth_syncing
		//
		// Undocumented APIs:
		// - eth_feeHistory
		// - eth_maxPriorityFeePerGas
		{"eth", ethapi.NewEthereumAPI(b)},
		// Standard Ethereum node APIs:
		// - eth_blockNumber
		// - eth_call
		// - eth_chainId
		// - eth_estimateGas
		// - eth_getBalance
		// - eth_getBlockByHash
		// - eth_getBlockByNumber
		// - eth_getCode
		// - eth_getStorageAt
		// - eth_getUncleByBlockHashAndIndex
		// - eth_getUncleByBlockNumberAndIndex
		// - eth_getUncleCountByBlockHash
		// - eth_getUncleCountByBlockNumber
		//
		// Geth-specific APIs:
		// - eth_createAccessList
		// - eth_getHeaderByHash
		// - eth_getHeaderByNumber
		//
		// Undocumented APIs:
		// - eth_getBlockReceipts
		// - eth_getProof
		{"eth", ethapi.NewBlockChainAPI(b)},
		// Standard Ethereum node APIs:
		// - eth_getBlockTransactionCountByHash
		// - eth_getBlockTransactionCountByNumber
		// - eth_getTransactionByBlockHashAndIndex
		// - eth_getTransactionByBlockNumberAndIndex
		// - eth_getTransactionByHash
		// - eth_getTransactionCount
		// - eth_getTransactionReceipt
		// - eth_sendRawTransaction
		// - eth_sendTransaction
		// - eth_sign
		// - eth_signTransaction
		//
		// Undocumented APIs:
		// - eth_fillTransaction
		// - eth_getRawTransactionByBlockHashAndIndex
		// - eth_getRawTransactionByBlockNumberAndIndex
		// - eth_getRawTransactionByHash
		// - eth_pendingTransactions
		// - eth_resend
		{"eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))},
		// Standard Ethereum node APIs:
		// - eth_getFilterChanges
		// - eth_getFilterLogs
		// - eth_getLogs
		// - eth_newBlockFilter
		// - eth_newFilter
		// - eth_newPendingTransactionFilter
		// - eth_uninstallFilter
		//
		// Geth-specific APIs:
		// - eth_subscribe
		//  - logs
		//  - newHeads
		//  - newPendingTransactions
		{"eth", filterAPI},
		// Geth-specific APIs:
		// - txpool_content
		// - txpool_contentFrom
		// - txpool_inspect
		// - txpool_status
		{"txpool", ethapi.NewTxPoolAPI(b)},
		// Geth-specific APIs:
		// - debug_chainDbCompact
		// - debug_chainDbProperty
		// - debug_dbAncient
		// - debug_dbAncients
		// - debug_dbGet
		// - debug_getRawBlock
		// - debug_getRawHeader
		// - debug_getRawReceipts
		// - debug_getRawTransaction
		// - debug_printBlock
		// - debug_setHead
		{"debug", ethapi.NewDebugAPI(b)},
		// Geth-specific APIs:
		// - debug_intermediateRoots
		// - debug_standardTraceBadBlockToFile
		// - debug_standardTraceBlockToFile
		// - debug_traceBadBlock
		// - debug_traceBlock
		// - debug_traceBlockByHash
		// - debug_traceBlockByNumber
		// - debug_traceBlockFromFile
		// - debug_traceCall
		// - debug_traceChain
		// - debug_traceTransaction
		{"debug", tracers.NewAPI(b)},
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
		{"debug", debug.Handler},
	}
	// Unsupported APIs:
	//
	// Standard Ethereum node APIs:
	// - eth_protocolVersion
	// - eth_coinbase
	// - eth_mining
	// - eth_hashrate
	// - eth_accounts
	//
	// Block and state inspection APIs:
	// - debug_accountRange
	// - debug_dumpBlock
	// - debug_getAccessibleState
	// - debug_getBadBlocks
	// - debug_getModifiedAccountsByHash
	// - debug_getModifiedAccountsByNumber
	// - debug_getTrieFlushInterval
	// - debug_preimage
	// - debug_setTrieFlushInterval
	// - debug_storageRangeAt
	//
	// The admin namespace.
	// The clique namespace.
	// The les namespace.
	// The miner namespace.
	// The personal namespace.
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
		clientVersion: version.Current.String(),
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

func (s *netAPI) Version() string {
	return s.chainID
}

func (s *netAPI) Listening() bool {
	return true // The node is always listening for p2p connections.
}

func (s *netAPI) PeerCount() hexutil.Uint {
	return hexutil.Uint(s.peers.Len()) //nolint:gosec // Peer count will not overflow
}

var (
	_ filters.Backend = (*apiBackend)(nil)
	_ ethapi.Backend  = (*apiBackend)(nil)
	_ tracers.Backend = (*apiBackend)(nil)
)

type apiBackend struct {
	*txgossip.Set
	vm             *VM
	accountManager *accounts.Manager
}

func (a *apiBackend) SyncProgress() ethereum.SyncProgress {
	// Avalanchego does not expose APIs until after the node has fully synced.
	// Just report that syncing is complete.
	return ethereum.SyncProgress{}
}

func (a *apiBackend) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) FeeHistory(ctx context.Context, blockCount uint64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) ChainDb() ethdb.Database {
	return a.vm.db
}

func (a *apiBackend) AccountManager() *accounts.Manager {
	return a.accountManager
}

func (a *apiBackend) ExtRPCEnabled() bool {
	// We never recommend to expose the RPC externally. Additionally, this is
	// only used as an additional security measure for the personal API, which
	// we do not support in its entirety.
	return false
}

func (a *apiBackend) RPCGasCap() uint64 {
	// TODO(StephenButtolph) Expose this as a config.
	return 25_000_000
}

func (a *apiBackend) RPCEVMTimeout() time.Duration {
	// TODO(StephenButtolph) Expose this as a config.
	return 5 * time.Second
}

func (a *apiBackend) RPCTxFeeCap() float64 {
	// TODO(StephenButtolph) Expose this as a config.
	return 1 // 1 AVAX
}

func (a *apiBackend) UnprotectedAllowed() bool {
	// TODO(StephenButtolph) Expose this as a config and default to false.
	return true
}

func (a *apiBackend) SetHead(number uint64) {
	// SAE does not support reorgs. We ignore any attempts to override the chain
	// head.
	a.vm.log().Warn("ignoring attempt to override the chain head",
		zap.Uint64("number", number),
	)
}

func (a *apiBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) HeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) CurrentHeader() *types.Header {
	panic(errUnimplemented)
}

func (a *apiBackend) CurrentBlock() *types.Header {
	return a.vm.exec.LastExecuted().Header()
}

func (a *apiBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	num, err := a.resolveBlockNumber(n)
	if errors.Is(err, errFutureBlockNotResolved) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return rawdb.ReadBlock(
		a.vm.db,
		rawdb.ReadCanonicalHash(a.vm.db, num),
		num,
	), nil
}

var errFutureBlockNotResolved = errors.New("not accepted yet")

func (a *apiBackend) resolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := a.vm.last.accepted.Load().Height()

	switch bn {
	case rpc.PendingBlockNumber: // i.e. pending execution
		return head, nil
	case rpc.LatestBlockNumber:
		return a.vm.exec.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return a.vm.last.settled.Load().Height(), nil
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

func (a *apiBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) StateAndHeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*state.StateDB, *types.Header, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) StateAndHeaderByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	panic(errUnimplemented)
}

func (a *apiBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) GetTd(ctx context.Context, hash common.Hash) *big.Int {
	return big.NewInt(0) // TODO(arr4n)
}

func (a *apiBackend) GetEVM(ctx context.Context, msg *core.Message, state *state.StateDB, header *types.Header, vmConfig *vm.Config, blockCtx *vm.BlockContext) *vm.EVM {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return a.vm.exec.SubscribeChainEvent(ch)
}

func (a *apiBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return a.vm.exec.SubscribeChainHeadEvent(ch)
}

func (*apiBackend) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	// SAE never reorgs, so there are no side events.
	return newNoopSubscription()
}

func (a *apiBackend) GetTransaction(ctx context.Context, txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) GetPoolTransactions() (types.Transactions, error) {
	pending := a.Pool.Pending(txpool.PendingFilter{})

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

func (a *apiBackend) GetPoolTransaction(txHash common.Hash) *types.Transaction {
	return a.Pool.Get(txHash)
}

func (a *apiBackend) GetPoolNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return a.Pool.Nonce(addr), nil
}

func (a *apiBackend) Stats() (pending int, queued int) {
	return a.Pool.Stats()
}

func (a *apiBackend) TxPoolContent() (map[common.Address][]*types.Transaction, map[common.Address][]*types.Transaction) {
	return a.Pool.Content()
}

func (a *apiBackend) TxPoolContentFrom(addr common.Address) ([]*types.Transaction, []*types.Transaction) {
	return a.Pool.ContentFrom(addr)
}

func (a *apiBackend) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return a.Set.Pool.SubscribeTransactions(ch, true)
}

func (a *apiBackend) ChainConfig() *params.ChainConfig {
	return a.vm.exec.ChainConfig()
}

func (a *apiBackend) Engine() consensus.Engine {
	panic(errUnimplemented)
}

func (a *apiBackend) GetBody(ctx context.Context, hash common.Hash, number rpc.BlockNumber) (*types.Body, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) GetLogs(ctx context.Context, blockHash common.Hash, number uint64) ([][]*types.Log, error) {
	panic(errUnimplemented)
}

func (*apiBackend) SubscribeRemovedLogsEvent(chan<- core.RemovedLogsEvent) event.Subscription {
	// SAE never reorgs, so no logs are ever removed.
	return newNoopSubscription()
}

func (a *apiBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return a.vm.exec.SubscribeLogsEvent(ch)
}

func (a *apiBackend) SubscribePendingLogsEvent(ch chan<- []*types.Log) event.Subscription {
	// In SAE, "pending" refers to the execution status. There are no logs known
	// for transactions pending execution.
	return newNoopSubscription()
}

func (a *apiBackend) BloomStatus() (uint64, uint64) {
	panic(errUnimplemented)
}

func (a *apiBackend) ServiceFilter(ctx context.Context, session *bloombits.MatcherSession) {
	panic(errUnimplemented)
}

func (a *apiBackend) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, tracers.StateReleaseFunc, error) {
	panic(errUnimplemented)
}

func (a *apiBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (*core.Message, vm.BlockContext, *state.StateDB, tracers.StateReleaseFunc, error) {
	panic(errUnimplemented)
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
