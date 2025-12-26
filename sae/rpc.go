// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
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
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/eth/filters"
	"github.com/ava-labs/libevm/eth/tracers"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/libevm/debug"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/node"
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
	apis := []struct {
		namespace string
		api       any
	}{
		// https://ethereum.org/developers/docs/apis/json-rpc/#web3_clientversion
		// https://ethereum.org/developers/docs/apis/json-rpc/#web3_sha3
		{"web3", node.NewWeb3API(version.Current.String())},
		// https://ethereum.org/developers/docs/apis/json-rpc#net_version
		// https://ethereum.org/developers/docs/apis/json-rpc#net_listening
		// https://ethereum.org/developers/docs/apis/json-rpc#net_peercount
		//
		// TODO: Populate p2p.Peers once we have P2P networking integrated.
		{"net", newNetAPI(nil, vm.exec.ChainConfig().ChainID.Uint64())},
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_syncing
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gasprice
		{"eth", ethapi.NewEthereumAPI(b)},
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_chainid
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_blocknumber
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getbalance
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getstorageat
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getunclecountbyblockhash
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getunclecountbyblocknumber
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getcode
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_call
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_estimategas
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getblockbyhash
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getblockbynumber
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getunclebyblockhashandindex
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getunclebyblocknumberandindex
		//
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-eth#eth-createaccesslist
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-eth#ethgetheaderbynumber
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-eth#ethgetheaderbyhash
		{"eth", ethapi.NewBlockChainAPI(b)},
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gettransactioncount
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getblocktransactioncountbyhash
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getblocktransactioncountbynumber
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_sign
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_signtransaction
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_sendtransaction
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_sendrawtransaction
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gettransactionbyhash
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gettransactionbyblockhashandindex
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gettransactionbyblocknumberandindex
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_gettransactionreceipt
		{"eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))},
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_newfilter
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_newblockfilter
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_newpendingtransactionfilter
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_uninstallfilter
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getfilterchanges
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getfilterlogs
		// https://ethereum.org/developers/docs/apis/json-rpc#eth_getlogs
		{"eth", filters.NewFilterAPI(filterSystem, false)},
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-txpool#txpool-content
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-txpool#txpool-contentfrom
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-txpool#txpool-inspect
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-txpool#txpool-status
		{"txpool", ethapi.NewTxPoolAPI(b)},
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugchaindbcompact
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugchaindbproperty
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugdbancient
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugdbancients
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugdbget
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetrawblock
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetrawheader
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetrawtransaction
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetrawreceipts
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugprintblock
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugsethead
		{"debug", ethapi.NewDebugAPI(b)},
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugintermediateroots
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstandardtraceblocktofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstandardtracebadblocktofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtracebadblock
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblock
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblockbynumber
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblockbyhash
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtraceblockfromfile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtracecall
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtracechain
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugtracetransaction
		{"debug", tracers.NewAPI(b)},
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugblockprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugcpuprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugfreeosmemory
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggcstats
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggotrace
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugmemstats
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugmutexprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugsetblockprofilerate
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugsetgcpercent
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugsetmutexprofilefraction
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstacks
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstartcpuprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstartgotrace
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstopcpuprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstopgotrace
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugverbosity
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugvmodule
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugwriteblockprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugwritememprofile
		// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugwritemutexprofile
		{"debug", debug.Handler},
	}
	// Unsupported APIs:
	//
	// "Standard" Ethereum node APIs:
	// https://ethereum.org/developers/docs/apis/json-rpc#eth_protocolversion
	// https://ethereum.org/developers/docs/apis/json-rpc#eth_coinbase
	// https://ethereum.org/developers/docs/apis/json-rpc#eth_mining
	// https://ethereum.org/developers/docs/apis/json-rpc#eth_hashrate
	// https://ethereum.org/developers/docs/apis/json-rpc#eth_accounts
	//
	// Block and state inspection APIs:
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugaccountrange
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugdumpblock
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetaccessiblestate
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetbadblocks
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetmodifiedaccountsbyhash
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggetmodifiedaccountsbynumber
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debuggettrieflushinterval
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugpreimage
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugsettrieflushinterval
	// https://geth.ethereum.org/docs/interacting-with-geth/rpc/ns-debug#debugstoragerangeat
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
	// TODO(StephenButtolph) Once [avalanchego#4792] is merged, we should report
	// the correct number of peers.
	return 0
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
	return a.vm.exec.DB()
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

func (a *apiBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	panic(errUnimplemented)
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
	panic(errUnimplemented)
}

func (a *apiBackend) GetEVM(ctx context.Context, msg *core.Message, state *state.StateDB, header *types.Header, vmConfig *vm.Config, blockCtx *vm.BlockContext) *vm.EVM {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	panic(errUnimplemented)
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

func (a *apiBackend) SubscribeNewTxsEvent(chan<- core.NewTxsEvent) event.Subscription {
	panic(errUnimplemented)
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

func (a *apiBackend) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	panic(errUnimplemented)
}

func (a *apiBackend) SubscribePendingLogsEvent(ch chan<- []*types.Log) event.Subscription {
	panic(errUnimplemented)
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
