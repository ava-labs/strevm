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
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/saexec"
)

var VMID = ids.ID{'s', 't', 'r', 'e', 'v', 'm'}

// VM implements Streaming Asynchronous Execution (SAE) of EVM blocks. It
// implements all [adaptor.ChainVM] methods except for `Initialize()`, which
// MUST be handled by a harness implementation that provides the final
// synchronous block, which MAY be a standard genesis block.
type VM struct {
	snowCtx *snow.Context
	snowcommon.AppHandler
	toEngine chan<- snowcommon.Message
	hooks    hook.Points
	now      func() time.Time

	consensusState utils.Atomic[snow.State]

	blocks     sink.Mutex[blockMap]
	preference atomic.Pointer[blocks.Block]
	last       last

	db ethdb.Database

	newTxs  chan *types.Transaction
	mempool sink.PriorityMutex[*queue.Priority[*pendingTx]]

	exec *saexec.Executor

	quit             chan struct{}
	mempoolAndExecWG sync.WaitGroup
}

type (
	blockMap map[common.Hash]*blocks.Block
	last     struct {
		synchronous                 struct{ height, time uint64 }
		accepted, executed, settled atomic.Pointer[blocks.Block]
	}
)

type Config struct {
	Hooks hook.Points
	// LastExecutedBlockHeight should be >= the LastSynchronousBlock height.
	//
	// TODO(StephenButtolph): This allows coreth to specify what atomic txs
	// (and warp receipts) have been applied. This is needed because the DB that
	// is written to with Hooks.BlockExecuted is not atomically managed with the
	// rest of SAE's state. We must ensure that Hooks.BlockExecuted is called
	// consecutively starting with the block with height
	// LastExecutedBlockHeight+1.
	LastExecutedBlockHeight uint64

	ChainConfig *params.ChainConfig
	DB          ethdb.Database
	// At the point of upgrade from synchronous to asynchronous execution, the
	// last synchronous block MUST be both the "head" and "finalized" block as
	// retrieved via [rawdb] functions and the database MUST NOT contain any
	// canonical hashes at greater heights. Its state root MUST be committed to
	// disk as database recovery can't re-execute it nor its ancestry in the
	// event of a node restart.
	LastSynchronousBlock LastSynchronousBlock

	ToEngine chan<- snowcommon.Message
	SnowCtx  *snow.Context

	// Now is optional, defaulting to [time.Now] if nil.
	Now func() time.Time
}

type LastSynchronousBlock struct {
	Hash                common.Hash
	Target, ExcessAfter gas.Gas
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
	recovery, err := vm.recoverFromDB(ctx, c.ChainConfig)
	if err != nil {
		return nil, err
	}
	if err := vm.startExecutorAndMempool(c.ChainConfig); err != nil {
		return nil, err
	}
	// We only commit the state root if [shouldCommitTrieDB] returns true for an
	// accepted block height. Therefore, even though we may have receipts and
	// other post-execution state in the database, the executor could only open
	// the last committed root.
	if err := vm.reexecuteBlocksAfterShutdown(ctx, recovery); err != nil {
		return nil, err
	}
	return vm, nil
}

func (vm *VM) startExecutorAndMempool(chainConfig *params.ChainConfig) error {
	exec, err := saexec.New(&vm.last.executed, chainConfig, vm.db, vm.hooks, vm.logger())
	if err != nil {
		return err
	}
	vm.exec = exec

	wg := &vm.mempoolAndExecWG
	wg.Add(2)
	execReady := make(chan struct{})
	go func() {
		vm.exec.Run(vm.quit, execReady)
		wg.Done()
	}()
	go func() {
		vm.startMempool()
		wg.Done()
	}()
	<-execReady

	return nil
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
	vm.logger().Info("Shutting down VM")
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

func (vm *VM) CreateHTTP2Handler(context.Context) (http.Handler, error) {
	return nil, errUnimplemented
}

func (vm *VM) GetBlock(ctx context.Context, blkID ids.ID) (*blocks.Block, error) {
	b, err := sink.FromMutex(ctx, vm.blocks, func(blocks blockMap) (*blocks.Block, error) {
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

func (vm *VM) ParseBlock(ctx context.Context, blockBytes []byte) (*blocks.Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(blockBytes, b); err != nil {
		return nil, err
	}
	// TODO(arr4n) do we need to populate parent and last-settled blocks here?
	// They will be populated by [VM.VerifyBlock] so I assume not, but best to
	// confirm and document here.
	return vm.newBlock(b, nil, nil)
}

func (vm *VM) BuildBlock(ctx context.Context) (*blocks.Block, error) {
	return vm.BuildBlockWithContext(ctx, nil)
}

func (vm *VM) BuildBlockWithContext(ctx context.Context, blockContext *block.Context) (*blocks.Block, error) {
	return vm.buildBlock(ctx, blockContext, uint64(vm.now().Unix()), vm.preference.Load())
}

func (vm *VM) signer(blockNum, timestamp uint64) types.Signer {
	return types.MakeSigner(vm.exec.ChainConfig(), new(big.Int).SetUint64(blockNum), timestamp)
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
