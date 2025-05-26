package sae

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
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
	now func() time.Time

	blocks           sink.Mutex[blockMap]
	accepted         sink.Mutex[*accepted]
	preference       sink.Mutex[ids.ID]
	genesisTimestamp uint64

	db ethdb.Database

	newTxs  chan *types.Transaction
	mempool sink.PriorityMutex[*queue.Priority[*pendingTx]]

	exec *executor

	quit             chan struct{}
	mempoolAndExecWG sync.WaitGroup
}

type blockMap map[ids.ID]*Block

type accepted struct {
	lastID     ids.ID
	all        blockMap
	heightToID map[uint64]ids.ID
}

func (a *accepted) last() *Block {
	return a.all[a.lastID]
}

func New(now func() time.Time) *VM {
	quit := make(chan struct{})

	vm := &VM{
		// VM
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		now:        now,
		blocks:     sink.NewMutex(make(blockMap)),
		accepted: sink.NewMutex(&accepted{
			all:        make(blockMap),
			heightToID: make(map[uint64]ids.ID),
		}),
		preference: sink.NewMutex[ids.ID](ids.Empty),
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
	<-execReady

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
	if err := vm.accepted.Use(ctx, func(a *accepted) error {
		id := genBlock.ID()
		a.all[id] = genBlock
		a.lastID = id
		a.heightToID[0] = id
		return nil
	}); err != nil {
		return err
	}

	return vm.afterInitialize(ctx, toEngine)
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

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		vm.blocks.Close()
		wg.Done()
	}()
	go func() {
		vm.accepted.Close()
		wg.Done()
	}()
	go func() {
		vm.preference.Close()
		wg.Done()
	}()
	wg.Wait()

	vm.mempoolAndExecWG.Wait()
	return nil
}

func (vm *VM) Version(context.Context) (string, error) {
	return "0", nil
}

func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return map[string]http.Handler{
		"sae": vm.ethRPCHandler(),
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
	parent, err := sink.FromMutex(ctx, vm.accepted, func(a *accepted) (*Block, error) {
		return a.last(), nil
	})
	if err != nil {
		return nil, err
	}

	return vm.buildBlock(ctx, uint64(vm.now().Unix()), parent)
}

func (vm *VM) signer() types.Signer {
	return types.LatestSigner(vm.exec.chainConfig)
}

func (vm *VM) SetPreference(ctx context.Context, blkID ids.ID) error {
	return vm.preference.Replace(ctx, func(ids.ID) (ids.ID, error) {
		return blkID, nil
	})
}

func (vm *VM) LastAccepted(ctx context.Context) (ids.ID, error) {
	return sink.FromMutex(ctx, vm.accepted, func(a *accepted) (ids.ID, error) {
		return a.lastID, nil
	})
}

func (vm *VM) GetBlockIDAtHeight(ctx context.Context, height uint64) (ids.ID, error) {
	return sink.FromMutex(ctx, vm.accepted, func(a *accepted) (ids.ID, error) {
		id, ok := a.heightToID[height]
		if !ok {
			return ids.Empty, database.ErrNotFound
		}
		return id, nil
	})
}

func (*VM) Engine() consensus.Engine {
	return nil
}

func (*VM) GetHeader(ethcommon.Hash, uint64) *types.Header {
	panic(errUnimplemented)
}
