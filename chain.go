package sae

import (
	"context"
	"math/big"
	"net/http"
	"sync"
	"time"

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

	exec        *executor
	execRunning <-chan struct{}
	quitExecute chan<- struct{}
	execDone    <-chan struct{}

	// Development double (like a test double, but with alliteration)
	mempool chan *types.Transaction
}

type blockMap map[ids.ID]*Block

type accepted struct {
	last ids.ID
	all  blockMap
}

func New() *Chain {
	running := make(chan struct{})
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
		// Execution
		execRunning: running,
		quitExecute: quit,
		execDone:    done,
		// Development-only workarounds
		mempool: make(chan *types.Transaction, 1000), // buffered so tx creation can keep going => load++
	}

	chain.exec = &executor{
		chain:   chain,
		running: running,
		quit:    quit,
		done:    done,
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
	if err := c.exec.init(); err != nil {
		return err
	}
	go c.exec.start()
	<-c.execRunning

	genesis := c.newBlock(
		types.NewBlockWithHeader(&types.Header{
			Number: big.NewInt(0),
			Time:   0,
		}),
	)
	if err := genesis.Verify(ctx); err != nil {
		return err
	}
	return genesis.Accept(ctx)
}

func (c *Chain) newBlock(b *types.Block) *Block {
	return &Block{b, c}
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
	close(c.quitExecute)

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

	select {
	case <-c.execDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
		return a.all[a.last].b, nil
	})
	if err != nil {
		return nil, err
	}

	body := types.Body{
		Transactions: make([]*types.Transaction, 0, 100_000),
	}
	stop := time.After(10 * time.Millisecond)
BuildLoop:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case tx := <-c.mempool:
			body.Transactions = append(body.Transactions, tx)
		case <-stop:
			break BuildLoop
		}
	}

	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     new(big.Int).SetUint64(parent.NumberU64() + 1),
		Time:       parent.Time() + 1,
	}
	needStateRootAt := clippedSubtract(header.Time, stateRootDelaySeconds)

	root, err := sink.FromMonitor(ctx, c.exec.chunks,
		func(cs map[uint64]*chunk) bool {
			_, ok := cs[needStateRootAt]

			msg := "Waiting for historical state root"
			if ok {
				msg = "Received historical state root"
			}
			c.logger().Debug(
				msg,
				zap.Uint64("block_timestamp", header.Time),
				zap.Uint64("chunk_timestamp", needStateRootAt),
			)

			return ok
		},
		func(cs map[uint64]*chunk) (ethcommon.Hash, error) {
			return cs[needStateRootAt].stateRoot, nil
		},
	)
	header.Root = root

	return c.newBlock(
		types.NewBlockWithHeader(header).WithBody(body),
	), nil
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
