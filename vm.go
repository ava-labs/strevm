package sae

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

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
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/strevm/adaptor"
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

	blocks     sink.Mutex[blockMap]
	accepted   sink.Mutex[*accepted]
	preference sink.Mutex[ids.ID]

	builder            blockBuilder
	exec               *executor
	quit               chan<- struct{}
	builderAndExecDone <-chan struct{} // receive 2, not closed
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

func New() *VM {
	quit := make(chan struct{})
	done := make(chan struct{})

	vm := &VM{
		// VM
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		blocks:     sink.NewMutex(make(blockMap)),
		accepted: sink.NewMutex(&accepted{
			all:        make(blockMap),
			heightToID: make(map[uint64]ids.ID),
		}),
		preference: sink.NewMutex[ids.ID](ids.Empty),
		// Block building
		builder: blockBuilder{
			tranches: sink.NewMonitor(&tranches{
				accepted:      newRootTxTranche(),
				chunkTranches: make(map[uint64]*txTranche),
			}),
			mempool: sink.NewPriorityMutex(new(mempool)),
			newTxs:  make(chan *transaction, 1e6), // TODO(arr4n) make the buffer configurable
			quit:    quit,
			done:    done,
		},
		quit:               quit, // both builder and executor
		builderAndExecDone: done, // each will send on this
	}

	vm.exec = &executor{
		vm:   vm,
		quit: quit,
		done: done,
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
	vm.builder.log = chainCtx.Log
	go vm.builder.startMempool()

	gen := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, gen); err != nil {
		return err
	}
	genBlock, err := vm.exec.init(ctx, gen)
	if err != nil {
		return err
	}
	// The genesis block can't be processed until the [blockBuilder] is
	// initialised with access to the just-initialised [executor] snapshots.
	vm.builder.snaps = vm.exec.snaps // safe to copy a [sink.Mutex]
	if err := vm.VerifyBlock(ctx, genBlock); err != nil {
		return err
	}
	if err := vm.AcceptBlock(ctx, genBlock); err != nil {
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

	for range 2 {
		select {
		case <-vm.builderAndExecDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (vm *VM) Version(context.Context) (string, error) {
	return "0", nil
}

func (vm *VM) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return nil, nil
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
	parent, err := sink.FromMutex(ctx, vm.accepted, func(a *accepted) (*types.Block, error) {
		return a.last().Block, nil
	})
	if err != nil {
		return nil, err
	}

	timestamp := parent.Time() + 1
	needStateRootAt := clippedSubtract(timestamp, stateRootDelaySeconds)
	chunk, err := sink.FromMonitor(ctx, vm.exec.chunks,
		func(res *executionResults) bool {
			_, ok := res.chunks[needStateRootAt]

			msg := "Waiting for historical state root"
			if ok {
				msg = "Received historical state root"
			}
			vm.logger().Debug(
				msg,
				zap.Uint64("block_timestamp", timestamp),
				zap.Uint64("chunk_timestamp", needStateRootAt),
			)

			return ok
		},
		func(res *executionResults) (*chunk, error) {
			// TODO(arr4n) this needs to be all chunks since the last parent,
			// but only once blocks have >1s between them.
			return res.chunks[needStateRootAt], nil
		},
	)
	if err != nil {
		return nil, err
	}

	// TODO(arr4n) this needs to be done immediately upon filling of the chunk,
	// but only once the [blockBuilder] keeps chunk tranches for historical
	// lookback.
	if err := vm.builder.clearExecuted(ctx, chunk); err != nil {
		return nil, err
	}

	x := &vm.exec.executeScratchSpace // TODO(arr4n) don't access this directly
	tranche, evmBlock, err := vm.builder.build(ctx, parent, chunk, x.chainConfig, &x.gasConfig)
	if err != nil {
		return nil, err
	}
	// Block-building is the only time we add a [txTranche] at the same time as
	// constructing the [Block]. In all other instances it's done in
	// [Block.Verify].
	b := vm.newBlock(evmBlock)
	b.tranche = tranche
	return b, nil
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
