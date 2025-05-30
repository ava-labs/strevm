package sae

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

var _ adaptor.ChainVM[*Block] = nil

// VM implements Streaming Asynchronous Execution (SAE) of EVM blocks. It
// implements all [adaptor.ChainVM] methods except for `Initialize()`, which
// MUST be handled by a harness implementation to provide the final synchronous
// block (e.g. [SinceGenesis]).
type VM struct {
	snowCtx *snow.Context
	common.AppHandler
	toEngine chan<- common.Message
	now      func() time.Time

	blocks                   sink.Mutex[blockMap]
	preference               atomic.Pointer[Block]
	lastSynchronousBlockTime uint64

	last struct {
		accepted, executed, settled atomic.Pointer[Block]
	}

	db ethdb.Database

	newTxs  chan *types.Transaction
	mempool sink.PriorityMutex[*queue.Priority[*pendingTx]]

	exec *executor

	quit             chan struct{}
	mempoolAndExecWG sync.WaitGroup
}

type blockMap map[ids.ID]*Block

type Config struct {
	ChainConfig          *params.ChainConfig
	DB                   ethdb.Database
	LastSynchronousBlock ethcommon.Hash

	ToEngine chan<- snowcommon.Message
	SnowCtx  *snow.Context

	// Now is optional, defaulting to [time.Now] if nil.
	Now func() time.Time
}

func New(ctx context.Context, c Config) (*VM, error) {
	quit := make(chan struct{})

	vm := &VM{
		// VM
		snowCtx:    c.SnowCtx,
		db:         c.DB,
		toEngine:   c.ToEngine,
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		now:        c.Now,
		blocks:     sink.NewMutex(make(blockMap)),
		// Block building
		newTxs:  make(chan *types.Transaction, 10), // TODO(arr4n) make the buffer configurable
		mempool: sink.NewPriorityMutex(new(queue.Priority[*pendingTx])),
		quit:    quit, // both mempool and executor
	}
	if vm.now == nil {
		vm.now = time.Now
	}

	lastSyncNum := rawdb.ReadHeaderNumber(c.DB, c.LastSynchronousBlock)
	if lastSyncNum == nil {
		return nil, fmt.Errorf("read number of last synchronous block (%#x): %w", c.LastSynchronousBlock, database.ErrNotFound)
	}
	genesis := vm.newBlock(rawdb.ReadBlock(c.DB, c.LastSynchronousBlock, *lastSyncNum))
	if genesis.Block == nil {
		return nil, fmt.Errorf("read last synchronous block (%#x): %w", c.LastSynchronousBlock, database.ErrNotFound)
	}
	vm.lastSynchronousBlockTime = genesis.Time()
	vm.logger().Info(
		"Last synchronous block before SAE",
		zap.Uint64("timestamp", genesis.Time()),
		zap.Stringer("hash", genesis.Hash()),
	)

	vm.exec = &executor{
		quit:         quit,
		log:          vm.logger(),
		chainConfig:  c.ChainConfig,
		db:           vm.db,
		lastExecuted: &vm.last.executed,
	}
	if err := vm.exec.init(genesis); err != nil {
		return nil, err
	}

	genesis.accepted.Store(true)
	genesis.execution = &executionResults{
		by:            vm.exec.gasClock.clone(),
		stateRootPost: genesis.Root(),
	}
	genesis.executed.Store(true)

	if err := vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[genesis.ID()] = genesis
		return nil
	}); err != nil {
		return nil, err
	}
	vm.last.accepted.Store(genesis)
	vm.last.executed.Store(genesis)
	vm.last.settled.Store(genesis)

	wg := &vm.mempoolAndExecWG
	wg.Add(2)
	execReady := make(chan struct{})
	go func() {
		vm.exec.run(execReady)
		wg.Done()
	}()
	go func() {
		vm.startMempool()
		wg.Done()
	}()
	<-execReady
	return vm, nil
}

// inMemoryBlockCount tracks the number of blocks created with [VM.newBlock]
// that are yet to have their GC finalizers run. See block pruning at the end of
// [VM.AcceptBlock].
var inMemoryBlockCount atomic.Uint64

func (vm *VM) newBlock(b *types.Block) *Block {
	inMemoryBlockCount.Add(1)
	bb := &Block{
		Block: b,
	}
	runtime.SetFinalizer(bb, func(*Block) {
		inMemoryBlockCount.Add(math.MaxUint64) // -1
	})
	return bb
}

func (vm *VM) logger() logging.Logger {
	return vm.snowCtx.Log
}

func (vm *VM) HealthCheck(context.Context) (any, error) {
	return nil, nil
}

func (vm *VM) Connected(
	ctx context.Context,
	nodeID ids.NodeID,
	nodeVersion *version.Application,
) error {
	return nil
}

func (vm *VM) Disconnected(ctx context.Context, nodeID ids.NodeID) error {
	return nil
}

func (vm *VM) SetState(ctx context.Context, state snow.State) error {
	return nil
}

func (vm *VM) Shutdown(ctx context.Context) error {
	vm.logger().Debug("Shutting down VM")
	close(vm.quit)

	vm.blocks.Close()
	vm.mempoolAndExecWG.Wait()
	return nil
}

func (vm *VM) Version(context.Context) (string, error) {
	return "0", nil
}

const HTTPHandlerKey = "sae"

func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return map[string]http.Handler{
		HTTPHandlerKey: vm.ethRPCHandler(),
	}, nil
}

func (vm *VM) GetBlock(ctx context.Context, blkID ids.ID) (*Block, error) {
	return sink.FromMutex(ctx, vm.blocks, func(blocks blockMap) (*Block, error) {
		b, ok := blocks[blkID]
		if !ok {
			return nil, database.ErrNotFound
		}
		return b, nil
	})
}

func (vm *VM) ParseBlock(ctx context.Context, blockBytes []byte) (*Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(blockBytes, b); err != nil {
		return nil, err
	}
	return vm.newBlock(b), nil
}

func (vm *VM) BuildBlock(ctx context.Context) (*Block, error) {
	return vm.buildBlock(ctx, uint64(vm.now().Unix()), vm.last.accepted.Load())
}

func (vm *VM) signer(blockNum, timestamp uint64) types.Signer {
	return types.MakeSigner(vm.exec.chainConfig, new(big.Int).SetUint64(blockNum), timestamp)
}

func (vm *VM) currSigner() types.Signer {
	return vm.signer(vm.last.accepted.Load().NumberU64()+1, uint64(time.Now().Unix()))
}

func (vm *VM) SetPreference(ctx context.Context, blkID ids.ID) error {
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		b, ok := bm[blkID]
		if !ok {
			return database.ErrNotFound
		}
		vm.preference.Store(b)
		return nil
	})
}

func (vm *VM) LastAccepted(ctx context.Context) (ids.ID, error) {
	return vm.last.accepted.Load().ID(), nil
}

func (vm *VM) GetBlockIDAtHeight(ctx context.Context, height uint64) (ids.ID, error) {
	h := rawdb.ReadCanonicalHash(vm.db, height)
	if h == (ethcommon.Hash{}) {
		return ids.Empty, database.ErrNotFound
	}
	return ids.ID(h), nil
}

type chainContext struct{}

func (chainContext) Engine() consensus.Engine {
	return engine{}
}

type engine struct {
	consensus.Engine
}

func (engine) Author(h *types.Header) (ethcommon.Address, error) {
	return ethcommon.Address{}, nil
}

func (chainContext) GetHeader(ethcommon.Hash, uint64) *types.Header {
	panic(errUnimplemented)
}
