package sae

import (
	"context"
	"errors"
	"sync"

	"github.com/arr4n/sink"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/log"
	"github.com/ava-labs/libevm/params"
)

type (
	sig struct{}
	ack struct{}
)

type exec struct {
	chain *Chain

	blocks <-chan blockAcceptance
	chunks chan<- *chunk
	quit   <-chan sig
	done   chan<- ack

	q       sink.Mutex[[]*Block]
	pending chan sig
	spawned sync.WaitGroup
}

type blockAcceptance struct {
	// This Context MUST be ephemeral and immediately dropped at the end of the
	// `case` in which it is received. Snowman will call [Block.Accept] with a
	// Context, at which point the block will be sent over a channel, the
	// receiver of which requires a Context (this one). Without refactoring the
	// pattern to being a function call, there is no other way to propagate the
	// Context.
	ctx   context.Context
	block *Block
}

type chunk struct{}

func (e *exec) start() {
	e.q = sink.NewMutex[[]*Block](nil)
	e.pending = make(chan sig)
	e.spawn(e.enqueueAccepted)
	e.spawn(e.clearQueue)

	<-e.quit
	e.spawned.Wait()
	close(e.chunks)
	close(e.pending)
	e.q.Close()

	close(e.done)
}

func (e *exec) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
}

func (e *exec) enqueueAccepted() {
	for {
		select {
		case <-e.quit:
			return
		case a := <-e.blocks:
			e.q.Use(a.ctx, func(q []*Block) ([]*Block, error) {
				return append(q, a.block), nil
			})
			e.spawn(e.signalQueuePush)
		}
	}
}

func (e *exec) signalQueuePush() {
	select {
	case <-e.quit:
	case e.pending <- sig{}:
	}
}

func (e *exec) clearQueue() {
	ctx, cancel := context.WithCancel(context.Background())
	e.spawn(func() {
		<-e.quit
		cancel()
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.pending:
			var b *Block
			err := e.q.Use(ctx, func(q []*Block) ([]*Block, error) {
				b = q[0]
				return q[1:], nil
			})
			if errors.Is(err, context.Canceled) {
				return
			}
			if err := e.execute(b); err != nil {
				log.Error("Executing enqueued block", "error", err.Error(), "height", b.Height(), "ID", b.ID())
				return
			}
		}
	}
}

func (e *exec) execute(b *Block) error {
	gp := new(core.GasPool)
	var statedb *state.StateDB
	var usedGas uint64

	for _, tx := range b.b.Transactions() {
		core.ApplyTransaction(
			params.AllDevChainProtocolChanges,
			e.chain,
			nil, // `author` (is nil in [core.StateProcessor.Process])
			gp,
			statedb,
			b.b.Header(),
			tx,
			&usedGas,
			vm.Config{},
		)
	}
	return nil
}
