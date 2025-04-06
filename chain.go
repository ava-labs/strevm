package sae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/version"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"go.uber.org/zap"
)

func init() {
	var (
		chain *Chain
		_     common.VM         = chain
		_     block.Getter      = chain
		_     block.Parser      = chain
		_     block.ChainVM     = chain
		_     core.ChainContext = chain
	)
}

type Chain struct {
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
	lastID ids.ID
	all    blockMap
}

func (a *accepted) last() *Block {
	return a.all[a.lastID]
}

func New() *Chain {
	quit := make(chan struct{})
	done := make(chan struct{})

	chain := &Chain{
		// Chain and chain state
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		blocks:     sink.NewMutex(make(blockMap)),
		accepted: sink.NewMutex(&accepted{
			all: make(blockMap),
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

	chain.exec = &executor{
		chain: chain,
		quit:  quit,
		done:  done,
	}

	return chain
}

func (c *Chain) Initialize(
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
	c.snowCtx = chainCtx
	c.builder.log = chainCtx.Log
	go c.builder.startMempool()

	gen := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, gen); err != nil {
		return err
	}
	genBlock, err := c.exec.init(ctx, gen)
	if err != nil {
		return err
	}
	// The genesis block can't be processed until the [blockBuilder] is
	// initialised with access to the just-initialised [executor] snapshots.
	c.builder.snaps = c.exec.snaps // safe to copy a [sink.Mutex]
	if err := genBlock.Verify(ctx); err != nil {
		return err
	}
	return genBlock.Accept(ctx)
}

func (c *Chain) newBlock(b *types.Block) *Block {
	return &Block{
		b:     b,
		chain: c,
	}
}

func (c *Chain) logger() logging.Logger {
	return c.snowCtx.Log
}

func (c *Chain) HealthCheck(context.Context) (any, error) {
	return nil, nil
}

func (c *Chain) Connected(
	ctx context.Context,
	nodeID ids.NodeID,
	nodeVersion *version.Application,
) error {
	return errUnimplemented
}

func (c *Chain) Disconnected(ctx context.Context, nodeID ids.NodeID) error {
	return errUnimplemented
}

func (c *Chain) SetState(ctx context.Context, state snow.State) error {
	return nil
}

func (c *Chain) Shutdown(ctx context.Context) error {
	c.logger().Debug("Shutting down VM")
	close(c.quit)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		c.blocks.Close()
		wg.Done()
	}()
	go func() {
		c.accepted.Close()
		wg.Done()
	}()
	go func() {
		c.preference.Close()
		wg.Done()
	}()
	wg.Wait()

	for range 2 {
		select {
		case <-c.builderAndExecDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *Chain) Version(context.Context) (string, error) {
	return "0", nil
}

func (c *Chain) CreateHandlers(context.Context) (map[string]http.Handler, error) {
	return nil, nil
}

func (c *Chain) GetBlock(ctx context.Context, blkID ids.ID) (snowman.Block, error) {
	return sink.FromMutex(ctx, c.blocks, func(blocks blockMap) (snowman.Block, error) {
		b, ok := blocks[blkID]
		if !ok {
			return nil, database.ErrNotFound
		}
		return b, nil
	})
}

func (c *Chain) ParseBlock(ctx context.Context, blockBytes []byte) (snowman.Block, error) {
	b := new(types.Block)
	if err := rlp.DecodeBytes(blockBytes, b); err != nil {
		return nil, err
	}
	return c.newBlock(b), nil
}

func (c *Chain) BuildBlock(ctx context.Context) (snowman.Block, error) {
	parent, err := sink.FromMutex(ctx, c.accepted, func(a *accepted) (*types.Block, error) {
		return a.last().b, nil
	})
	if err != nil {
		return nil, err
	}

	timestamp := parent.Time() + 1
	needStateRootAt := clippedSubtract(timestamp, stateRootDelaySeconds)
	chunk, err := sink.FromMonitor(ctx, c.exec.chunks,
		func(res *executionResults) bool {
			_, ok := res.chunks[needStateRootAt]

			msg := "Waiting for historical state root"
			if ok {
				msg = "Received historical state root"
			}
			c.logger().Debug(
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

	// TODO(arr4n) this needs to be done immediately upon filling of the chunk,
	// but only once the [blockBuilder] keeps chunk tranches for historical
	// lookback.
	if err := c.builder.clearExecuted(ctx, chunk); err != nil {
		return nil, err
	}

	x := &c.exec.executeScratchSpace // TODO(arr4n) don't access this directly
	tranche, evmBlock, err := c.builder.build(ctx, parent, chunk, x.chainConfig, &x.gasConfig)
	if err != nil {
		return nil, err
	}
	// Block-building is the only time we add a [txTranche] at the same time as
	// constructing the [Block]. In all other instances it's done in
	// [Block.Verify].
	b := c.newBlock(evmBlock)
	b.tranche = tranche
	return b, nil
}

func (c *Chain) SetPreference(ctx context.Context, blkID ids.ID) error {
	return c.preference.Replace(ctx, func(ids.ID) (ids.ID, error) {
		return blkID, nil
	})
}

func (c *Chain) LastAccepted(context.Context) (ids.ID, error) {
	return ids.ID{}, errUnimplemented
}

func (c *Chain) GetBlockIDAtHeight(ctx context.Context, height uint64) (ids.ID, error) {
	return ids.Empty, errUnimplemented
}

func (*Chain) Engine() consensus.Engine {
	return nil
}

func (c *Chain) GetHeader(ethcommon.Hash, uint64) *types.Header {
	panic(errUnimplemented)
}
