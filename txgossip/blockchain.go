// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package txgossip

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/txpool"
	"github.com/ava-labs/libevm/core/txpool/legacypool"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/event"
	"github.com/ava-labs/libevm/params"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saexec"
)

// A BlockChain is the union of [txpool.BlockChain] and [legacypool.BlockChain].
type BlockChain interface {
	txpool.BlockChain
	legacypool.BlockChain
}

// NewBlockChain wraps an [saexec.Executor] to be compatible with a
// non-blob-transaction mempool.
func NewBlockChain(exec *saexec.Executor, blocks blocks.Source) BlockChain {
	return &blockchain{
		exec:   exec,
		blocks: blocks,
	}
}

type blockchain struct {
	exec   *saexec.Executor
	blocks blocks.Source
}

func (bc *blockchain) Config() *params.ChainConfig {
	return bc.exec.ChainConfig()
}

func (bc *blockchain) CurrentBlock() *types.Header {
	return bc.exec.LastEnqueued().Header()
}

func (bc *blockchain) GetBlock(hash common.Hash, number uint64) *types.Block {
	return bc.blocks.EthBlock(hash, number)
}

func (bc *blockchain) StateAt(root common.Hash) (*state.StateDB, error) {
	return state.New(root, bc.exec.StateCache(), nil)
}

// SubscribeChainHeadEvent subscribes to block enqueueing, NOT to regular head
// events as these only occur after execution. Enqueuing is equivalent to block
// acceptance, which is when a transaction SHOULD be removed from the mempool.
func (bc *blockchain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	bCh := make(chan *types.Block)
	sub := bc.exec.SubscribeBlockEnqueueEvent(bCh)

	p := &pipe{
		in:   bCh,
		out:  ch,
		sub:  sub,
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go p.loop()
	return p
}

// A pipe is an [event.Subscription] that converts from a [types.Block] channel
// to a [core.ChainHeadEvent] one.
type pipe struct {
	in         <-chan *types.Block
	out        chan<- core.ChainHeadEvent
	sub        event.Subscription
	quit, done chan struct{}
}

func (p *pipe) loop() {
	defer close(p.done)
	for {
		select {
		case b := <-p.in:
			select {
			case p.out <- core.ChainHeadEvent{Block: b}:

			case <-p.quit:
				return
			}
		case <-p.quit:
			return
		}
	}
}

func (p *pipe) Err() <-chan error {
	return p.sub.Err()
}

func (p *pipe) Unsubscribe() {
	p.sub.Unsubscribe()
	close(p.quit)
	<-p.done
}
