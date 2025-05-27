package sae

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/queue"
	"go.uber.org/zap"
)

func init() {
	var (
		vm *VM
		_  common.VM               = vm
		_  core.ChainContext       = vm
		_  adaptor.ChainVM[*Block] = vm
	)
}

type VM struct {
	snowCtx *snow.Context
	common.AppHandler
	toEngine chan<- common.Message
	now      func() time.Time

	blocks           sink.Mutex[blockMap]
	preference       atomic.Pointer[Block]
	genesisTimestamp uint64

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

func New(now func() time.Time) *VM {
	quit := make(chan struct{})

	vm := &VM{
		// VM
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		now:        now,
		blocks:     sink.NewMutex(make(blockMap)),
		// Block building
		newTxs:  make(chan *types.Transaction, 10), // TODO(arr4n) make the buffer configurable
		mempool: sink.NewPriorityMutex(new(queue.Priority[*pendingTx])),
		quit:    quit, // both mempool and executor
	}

	vm.exec = &executor{
		vm:   vm,
		quit: quit,
	}

	return vm
}

func (vm *VM) Initialize(
	ctx context.Context,
	chainCtx *snow.Context,
	db database.Database,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- common.Message,
	fxs []*common.Fx,
	appSender common.AppSender,
) error {
	vm.snowCtx = chainCtx
	vm.db = rawdb.NewMemoryDatabase() // TODO(arr4n) wrap the [database.Database] argument
	vm.toEngine = toEngine

	gen := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, gen); err != nil {
		return err
	}
	genBlock, err := vm.exec.init(ctx, gen, vm.db)
	if err != nil {
		return err
	}
	vm.genesisTimestamp = gen.Timestamp

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
	defer func() { <-execReady }()

	vm.logger().Debug(
		"Genesis block",
		zap.Uint64("timestamp", genBlock.Time()),
		zap.Stringer("hash", genBlock.Hash()),
	)

	if err := vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[genBlock.ID()] = genBlock
		return nil
	}); err != nil {
		return err
	}
	vm.last.accepted.Store(genBlock)
	vm.last.executed.Store(genBlock)
	vm.last.settled.Store(genBlock)

	return nil
}

func (vm *VM) newBlock(b *types.Block) *Block {
	return &Block{
		Block: b,
	}
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

func (vm *VM) signer() types.Signer {
	return types.LatestSigner(vm.exec.chainConfig)
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

func (*VM) Engine() consensus.Engine {
	return engine{}
}

type engine struct {
	consensus.Engine
}

func (engine) Author(h *types.Header) (ethcommon.Address, error) {
	return ethcommon.Address{}, nil
}

func (*VM) GetHeader(ethcommon.Hash, uint64) *types.Header {
	panic(errUnimplemented)
}
