package sae

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sync"

	"github.com/arr4n/sink"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"
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

	spawned sync.WaitGroup

	q       sink.Mutex[[]*Block]
	pending chan sig

	gasConfig gas.Config
	excess    gas.Gas
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

type chunk struct {
	receipts []*types.Receipt
}

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

func (e *exec) logger() logging.Logger {
	return e.chain.snowCtx.Log
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
			e.q.Replace(a.ctx, func(q []*Block) ([]*Block, error) {
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
			err := e.q.Replace(ctx, func(q []*Block) ([]*Block, error) {
				b = q[0]
				return q[1:], ctx.Err()
			})
			if errors.Is(err, context.Canceled) {
				return
			}
			if err := e.execute(ctx, b); err != nil {
				e.logger().Error(
					"Executing enqueued block",
					zap.Error(err),
					zap.Uint64("height", b.Height()),
					zap.Any("hash", b.b.Hash()),
				)
				return
			}
		}
	}
}

func (e *exec) execute(ctx context.Context, b *Block) error {
	gp := new(core.GasPool)
	gp.SetGas(math.MaxUint64)

	db := state.NewDatabase(rawdb.NewMemoryDatabase())
	statedb, err := state.New(types.EmptyRootHash, db, nil)
	if err != nil {
		return err
	}
	var usedGas uint64

	hdr := types.CopyHeader(b.b.Header())
	hdr.BaseFee = new(big.Int)
	maxChunkSize := e.gasConfig.MaxPerSecond
	_ = maxChunkSize

	// TODO(arr4n): implement the chunk-boundary algorithm
	chunk := new(chunk)

	for i, tx := range b.b.Transactions() {
		price := gas.CalculatePrice(params.GWei, e.excess, e.gasConfig.ExcessConversionConstant)
		hdr.BaseFee.SetUint64(uint64(price))

		statedb.SetTxContext(tx.Hash(), i)

		receipt, err := core.ApplyTransaction(
			params.TestChainConfig,
			e.chain,
			&hdr.Coinbase,
			gp,
			statedb,
			hdr,
			tx,
			&usedGas,
			vm.Config{},
		)
		if err != nil {
			return err
		}
		chunk.receipts = append(chunk.receipts, receipt)
		e.excess += gas.Gas(receipt.GasUsed / 2)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.chunks <- chunk:
		return nil
	}
}
