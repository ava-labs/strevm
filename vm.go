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

type blockMap map[ethcommon.Hash]*Block

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

	genesis.execution = &executionResults{
		by:            vm.exec.gasClock.clone(),
		stateRootPost: genesis.Root(),
	}
	batch := vm.db.NewBatch()
	if err := genesis.writePostExecutionState(batch); err != nil {
		return nil, err
	}
	// The last synchronous block (the SAE "genesis") is, by definition, already
	// settled, so the `parent` and `lastSettled` pointers MUST be nil (see the
	// invariant documented in [Block]). We do, however, need to write the
	// last-settled block number to the database so set it temporarily.
	genesis.lastSettled = genesis
	defer func() { genesis.lastSettled = nil }()
	if err := genesis.writeLastSettledNumber(batch); err != nil {
		return nil, err
	}
	if err := batch.Write(); err != nil {
		return nil, err
	}
	genesis.executed.Store(true)

	if err := vm.blocks.Use(ctx, func(bm blockMap) error {
		bm[genesis.Hash()] = genesis
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

func (vm *VM) recoverFromDB(ctx context.Context) error {
	db := vm.db

	lastExecutedHeader := rawdb.ReadHeadHeader(db)
	lastExecutedHash := lastExecutedHeader.Hash()
	lastExecutedNum := lastExecutedHeader.Number.Uint64()

	lastSettledHash := rawdb.ReadFinalizedBlockHash(db)
	lastSettledNum := rawdb.ReadHeaderNumber(db, lastSettledHash)
	if lastSettledNum == nil {
		return fmt.Errorf("database in corrupt state: rawdb.ReadHeaderNumber(rawdb.ReadFinalizedBlockHash() = %#x) returned nil", lastSettledHash)
	}

	// Although we won't need all of these blocks, the last-settled of the
	// last-settled block is a guaranteed and reasonable lower bound on the
	// blocks to recover.
	from, err := vm.readLastSettledNumber(*lastSettledNum)
	if err != nil {
		return err
	}

	// In [VM.AcceptBlock] we prune all blocks before the last-settled one so
	// that's all we need to recover.
	nums, hashes := rawdb.ReadAllCanonicalHashes(vm.db, from, math.MaxUint64, math.MaxInt)

	// [rawdb.ReadAllCanonicalHashes] does not state that it returns blocks in
	// increasing numerical order. It might, but it doesn't say so on the tin.
	lastAcceptedNum := *lastSettledNum
	lastAcceptedHash := lastSettledHash
	for i, num := range nums {
		if num <= lastAcceptedNum {
			continue
		}
		lastAcceptedNum = num
		lastAcceptedHash = hashes[i]
	}

	return vm.blocks.Replace(ctx, func(blockMap) (blockMap, error) {
		byID := make(blockMap)
		byNum := make(map[uint64]*Block)

		for i, num := range nums {
			hash := hashes[i]
			b := vm.newBlock(rawdb.ReadBlock(db, hash, num))
			if b.Block == nil {
				return nil, fmt.Errorf("rawdb.ReadBlock(%#x, %d) returned nil", hash, num)
			}
			byID[b.Hash()] = b
			byNum[num] = b

			if num <= lastExecutedNum {
				state, err := vm.readPostExecutionState(num)
				if err != nil {
					return nil, fmt.Errorf("recover post-execution state of block %d: %w", num, err)
				}
				state.receipts = rawdb.ReadReceipts(db, b.Hash(), b.NumberU64(), b.Time(), vm.exec.chainConfig)
				b.execution = state
				b.executed.Store(true)
			}

			if b.Hash() == lastExecutedHash {
				vm.last.executed.Store(b)
			}
			if b.Hash() == lastSettledHash {
				vm.last.settled.Store(b)
			}
			if b.Hash() == lastAcceptedHash {
				vm.last.accepted.Store(b)
			}
		}
		vm.preference.Store(vm.last.accepted.Load())

		for _, b := range byID {
			num := b.NumberU64()
			if num <= *lastSettledNum {
				// Once a block is settled, its `parent` and `lastSettled`
				// pointers are cleared to allow for GC, so we don't reinstate
				// them here.
				continue
			}

			b.parent = byNum[num-1]
			s, err := vm.readLastSettledNumber(num)
			if err != nil {
				return nil, err
			}
			b.lastSettled = byNum[s]
		}

		for _, num := range nums {
			b := byNum[num]
			// These were only needed as the last-settled pointers of blocks
			// that themselves are yet to be settled.
			if num < *lastSettledNum {
				delete(byID, b.Hash())
			}
		}

		return byID, nil
	})
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

const (
	HTTPHandlerKey = "sae_http"
	WSHandlerKey   = "sae_ws"
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
	return sink.FromMutex(ctx, vm.blocks, func(blocks blockMap) (*Block, error) {
		b, ok := blocks[ethcommon.Hash(blkID)]
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
		b, ok := bm[ethcommon.Hash(blkID)]
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
