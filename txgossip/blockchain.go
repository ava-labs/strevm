// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"github.com/ava-labs/avalanchego/cache/lru"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

// A BlockChain is the union of [txpool.BlockChain] and [legacypool.BlockChain].
type BlockChain interface {
	txpool.BlockChain
	legacypool.BlockChain
}

// NewBlockChain wraps an [saexec.Executor] to be compatible with a
// non-blob-transaction mempool. The returned instance assumes that the
// consuming mempool expects a synchronous chain so its `StateAt()` method maps
// state roots from settled to the latest post-execution equivalents. The roots
// carried by [types.Header] instances will therefore not match their respective
// [state.StateDB] instances, which will instead be opened at
// [blocks.Block.PostExecutionStateRoot].
func NewBlockChain(log logging.Logger, exec *saexec.Executor, blocks blocks.Source) BlockChain {
	return &blockchain{
		log:                   log,
		exec:                  exec,
		blocks:                blocks,
		settledToExecutedRoot: lru.NewCache[common.Hash, common.Hash](64),
	}
}

type blockchain struct {
	log                   logging.Logger
	exec                  *saexec.Executor
	blocks                blocks.Source
	settledToExecutedRoot *lru.Cache[common.Hash, common.Hash]
}

func (bc *blockchain) Config() *params.ChainConfig {
	return bc.exec.ChainConfig()
}

// mapStateRoots populates the mapping of settled to executed state roots and
// returns the block, unchanged, which allows the method to be chained easily.
// Previously seen settled-state roots are overwritten to ensure that the
// mempool sees the latest possible executed state.
func (bc *blockchain) mapStateRoots(b *blocks.Block) *blocks.Block {
	if b != nil {
		bc.settledToExecutedRoot.Put(b.SettledStateRoot(), b.PostExecutionStateRoot())
	}
	return b
}

func (bc *blockchain) CurrentBlock() *types.Header {
	return bc.mapStateRoots(bc.exec.LastExecuted()).Header()
}

func (bc *blockchain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return bc.blocks.EthBlock(hash, number)
}

// StateAt is a filthy liar! Instead of opening the requested root, it assumes
// that it is a settled root and instead opens the latest known post-execution
// state root of a block that has the requested root as its last-settled state.
func (bc *blockchain) StateAt(root common.Hash) (*state.StateDB, error) {
	c := bc.settledToExecutedRoot
	if mapped, ok := c.Get(root); ok {
		root = mapped
	} else {
		bc.log.Warn("Received request to open unexpected state root", zap.Stringer("root", root))
	}
	return state.New(root, bc.exec.StateCache(), nil)
}

func (bc *blockchain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	bCh := make(chan *blocks.Block)
	sub := bc.exec.SubscribeBlockExecutionEvent(bCh)

	p := &pipe{
		in:      bCh,
		out:     ch,
		inspect: bc.mapStateRoots,
		sub:     sub,
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go p.loop()
	return p
}

// A pipe is an [event.Subscription] that converts from a [blocks.Block] channel
// to a [core.ChainHeadEvent] one, allowing for inspection of the former (akin
// to a tee but not via a channel).
type pipe struct {
	in         <-chan *blocks.Block
	out        chan<- core.ChainHeadEvent
	inspect    func(*blocks.Block) *blocks.Block
	sub        event.Subscription
	quit, done chan struct{}
}

func (p *pipe) Unsubscribe() {
	p.sub.Unsubscribe()
	close(p.quit)
	<-p.done
}

func (p *pipe) Err() <-chan error {
	return p.sub.Err()
}

func (p *pipe) loop() {
	for {
		b, ok := p.read()
		if !ok || !p.write(p.inspect(b)) {
			close(p.done)
			return
		}
	}
}

func (p *pipe) read() (*blocks.Block, bool) {
	select {
	case b := <-p.in:
		return b, true
	case <-p.quit:
		return nil, false
	}
}

func (p *pipe) write(b *blocks.Block) bool {
	select {
	case p.out <- core.ChainHeadEvent{Block: b.EthBlock()}:
		return true
	case <-p.quit:
		return false
	}
}
