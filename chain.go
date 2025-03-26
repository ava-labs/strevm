package sae

import (
	"context"
	"math/big"
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

	genesis    ids.ID
	blocks     sink.Mutex[blockMap]
	accepted   sink.Mutex[*accepted]
	preference sink.Mutex[ids.ID]

	exec        *executor
	quitExecute chan<- sig
	doneExecute <-chan ack
	toExecute   chan<- blockAcceptance
}

type blockMap map[ids.ID]*Block

type accepted struct {
	last ids.ID
	all  blockMap
}

func New() *Chain {
	blocks := make(chan blockAcceptance)
	quit := make(chan sig)
	done := make(chan ack)

	chain := &Chain{
		// Chain and chain state
		AppHandler: common.NewNoOpAppHandler(logging.NoLog{}),
		blocks:     sink.NewMutex(make(blockMap)),
		accepted: sink.NewMutex(&accepted{
			all: make(blockMap),
		}),
		preference: sink.NewMutex[ids.ID](ids.Empty),
		// Execution
		quitExecute: quit,
		doneExecute: done,
		toExecute:   blocks,
	}

	chain.exec = &executor{
		chain:    chain,
		accepted: blocks,
		quit:     quit,
		done:     done,
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
	genesis := &Block{
		b: types.NewBlockWithHeader(&types.Header{
			Number: big.NewInt(0),
			Time:   0,
		}),
		chain: c,
	}
	c.genesis = genesis.ID()
	c.blocks.Use(ctx, func(bs blockMap) error {
		bs[genesis.ID()] = genesis
		return nil
	})
	c.accepted.Use(ctx, func(a *accepted) error {
		a.last = genesis.ID()
		a.all[genesis.ID()] = genesis
		return nil
	})

	c.snowCtx = chainCtx
	if err := c.exec.init(); err != nil {
		return err
	}
	go c.exec.start()
	return genesis.Accept(ctx)
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
	case <-c.doneExecute:
		close(c.toExecute)
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
	return &Block{
		b:     b,
		chain: c,
	}, nil
}

func (c *Chain) BuildBlock(context.Context) (snowman.Block, error) {
	return nil, errUnimplemented
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
