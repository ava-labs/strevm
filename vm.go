package sae

import (
	"context"
	"math/big"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/queue"
)

// VM implements Streaming Asynchronous Execution (SAE) of EVM blocks. It
// implements all [adaptor.ChainVM] methods except for `Initialize()`, which
// MUST be handled by a harness implementation that provides the final
// synchronous block, which MAY be a standard genesis block.
type VM struct {
	snowCtx *snow.Context
	snowcommon.AppHandler
	toEngine chan<- snowcommon.Message
	hooks    Hooks
	now      func() time.Time

	consensusState utils.Atomic[snow.State]

	blocks     sink.Mutex[blockMap]
	preference atomic.Pointer[Block]
	last       last

	db ethdb.Database

	newTxs  chan *types.Transaction
	mempool sink.PriorityMutex[*queue.Priority[*pendingTx]]

	exec *executor

	quit             chan struct{}
	mempoolAndExecWG sync.WaitGroup
}

type (
	blockMap map[common.Hash]*Block
	last     struct {
		synchronousTime             uint64
		accepted, executed, settled atomic.Pointer[Block]
	}
)

type Config struct {
	Hooks       Hooks
	ChainConfig *params.ChainConfig
	DB          ethdb.Database
	// At the point of upgrade from synchronous to asynchronous execution, the
	// last synchronous block MUST be both the "head" and "finalized" block as
	// retrieved via [rawdb] functions and the database MUST NOT contain any
	// canonical hashes at greater heights.
	LastSynchronousBlock common.Hash

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
		hooks:      c.Hooks,
		AppHandler: snowcommon.NewNoOpAppHandler(logging.NoLog{}),
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

	if err := vm.upgradeLastSynchronousBlock(c.LastSynchronousBlock); err != nil {
		return nil, err
	}
	if err := vm.recoverFromDB(ctx, c.ChainConfig); err != nil {
		return nil, err
	}

	vm.exec = &executor{
		quit:         quit,
		log:          vm.logger(),
		hooks:        vm.hooks,
		chainConfig:  c.ChainConfig,
		db:           vm.db,
		lastExecuted: &vm.last.executed,
	}
	if err := vm.exec.init(); err != nil {
		return nil, err
	}

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

func (vm *VM) newBlock(b *types.Block, parent, lastSettled *Block) (*Block, error) {
	return blocks.New(b, parent, lastSettled, vm.logger())
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
	vm.consensusState.Set(state)
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

const (
	HTTPHandlerKey = "/sae/http"
	WSHandlerKey   = "/sae/ws"
)

func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	s := vm.ethRPCServer()
	return map[string]http.Handler{
		HTTPHandlerKey: s,
		WSHandlerKey: s.WebsocketHandler(
			[]string{"*"}, // TODO(arr4n) make this configurable
		),
	}, nil
}

func (vm *VM) GetBlock(ctx context.Context, blkID ids.ID) (*Block, error) {
	b, err := sink.FromMutex(ctx, vm.blocks, func(blocks blockMap) (*Block, error) {
		if b, ok := blocks[common.Hash(blkID)]; ok {
			return b, nil
		}
		return nil, database.ErrNotFound
	})
	switch err {
	case nil:
		return b, nil
	case database.ErrNotFound:
	default:
		return nil, err
	}

	hash := common.Hash(blkID)
	num := rawdb.ReadHeaderNumber(vm.db, hash)
	if num == nil {
		return nil, database.ErrNotFound
	}
	ethB := rawdb.ReadBlock(vm.db, hash, *num)
	if ethB == nil {
		return nil, database.ErrNotFound
	}
	// Not being in the blockMap (above) means that the block has been settled
	// and that its parent and last-settled pointers would have been cleared; we
	// therefore don't populate either of them.
	return vm.newBlock(ethB, nil, nil)
}

func (vm *VM) ParseBlock(ctx context.Context, blockBytes []byte) (*Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(blockBytes, b); err != nil {
		return nil, err
	}
	// TODO(arr4n) do we need to populate parent and last-settled blocks here?
	// They will be populated by [VM.VerifyBlock] so I assume not, but best to
	// confirm and document here.
	return vm.newBlock(b, nil, nil)
}

func (vm *VM) BuildBlock(ctx context.Context) (*Block, error) {
	return vm.buildBlock(ctx, uint64(vm.now().Unix()), vm.preference.Load())
}

func (vm *VM) signer(blockNum, timestamp uint64) types.Signer {
	return types.MakeSigner(vm.exec.chainConfig, new(big.Int).SetUint64(blockNum), timestamp)
}

func (vm *VM) currSigner() types.Signer {
	return vm.signer(vm.last.accepted.Load().NumberU64()+1, uint64(time.Now().Unix()))
}

func (vm *VM) SetPreference(ctx context.Context, blkID ids.ID) error {
	return vm.blocks.Use(ctx, func(bm blockMap) error {
		b, ok := bm[common.Hash(blkID)]
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
	if h == (common.Hash{}) {
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

func (engine) Author(h *types.Header) (common.Address, error) {
	return common.Address{}, nil
}

func (chainContext) GetHeader(common.Hash, uint64) *types.Header {
	panic(errUnimplemented)
}
