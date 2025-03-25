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
	"github.com/ava-labs/libevm/common"
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

type executor struct {
	chain *Chain

	accepted <-chan blockAcceptance
	chunks   chan<- *chunk
	quit     <-chan sig
	done     chan<- ack

	spawned sync.WaitGroup

	q        sink.Mutex[[]*Block]
	pending  chan sig
	statedb  *state.StateDB
	receipts chan *execResult

	gasConfig gas.Config
	excess    gas.Gas
}

func (e *executor) init() error {
	db := state.NewDatabase(rawdb.NewMemoryDatabase())
	statedb, err := state.New(types.EmptyRootHash, db, nil)
	if err != nil {
		return err
	}
	e.statedb = statedb
	return nil
}

func (e *executor) start() {
	e.q = sink.NewMutex[[]*Block](nil)
	e.pending = make(chan sig)
	e.receipts = make(chan *execResult, 1+uint64(e.gasConfig.MaxPerSecond)/params.TxGas)
	e.spawn(e.enqueueAccepted)
	e.spawn(e.clearQueue)
	e.spawn(e.fillChunks)

	<-e.quit
	e.spawned.Wait()
	close(e.chunks)
	close(e.pending)
	close(e.receipts)
	e.q.Close()

	close(e.done)
}

func (e *executor) logger() logging.Logger {
	return e.chain.snowCtx.Log
}

func (e *executor) spawn(fn func()) {
	e.spawned.Add(1)
	go func() {
		fn()
		e.spawned.Done()
	}()
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
	errCh chan error
}

func (e *executor) enqueueAccepted() {
	for {
		a, ok := recv(e.quit, e.accepted)
		if !ok {
			return
		}
		a.errCh <- e.q.Replace(a.ctx, func(q []*Block) ([]*Block, error) {
			q = append(q, a.block)
			e.spawn(e.signalQueuePush)
			return q, nil
		})
	}
}

func (e *executor) signalQueuePush() {
	send(e.quit, e.pending, sig{})
}

func (e *executor) clearQueue() {
	ctx, cancel := context.WithCancel(context.Background())
	e.spawn(func() {
		<-e.quit
		cancel()
	})

	for {
		if _, ok := recv(e.quit, e.pending); !ok {
			return
		}

		var b *Block
		err := e.q.Replace(ctx, func(q []*Block) ([]*Block, error) {
			b = q[0]
			return q[1:], ctx.Err()
		})
		if errors.Is(err, context.Canceled) {
			return
		}

		switch err := e.execute(b); err {
		case nil:
		default:
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

func (e *executor) execute(b *Block) error {
	gp := new(core.GasPool)
	gp.SetGas(math.MaxUint64)

	var usedGas uint64

	hdr := types.CopyHeader(b.b.Header())
	hdr.BaseFee = new(big.Int)
	maxChunkSize := e.gasConfig.MaxPerSecond
	_ = maxChunkSize

	txs := b.b.Transactions()
	lastTxIndex := len(txs) - 1
	for ti, tx := range txs {
		price := gas.CalculatePrice(params.GWei, e.excess, e.gasConfig.ExcessConversionConstant)
		hdr.BaseFee.SetUint64(uint64(price))

		e.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			params.TestChainConfig,
			e.chain,
			&hdr.Coinbase,
			gp,
			e.statedb,
			hdr,
			tx,
			&usedGas,
			vm.Config{},
		)
		if err != nil {
			return err
		}

		res := &execResult{
			receipt:     receipt,
			lastInBlock: ti == lastTxIndex,
		}
		if !send(e.quit, e.receipts, res) {
			return context.Canceled
		}
		e.excess += gas.Gas(receipt.GasUsed / 2)
	}

	return nil
}

type chunk struct {
	stateRoot common.Hash
	receipts  []*types.Receipt
}

type execResult struct {
	receipt     *types.Receipt
	lastInBlock bool
}

func (e *executor) fillChunks() {
	c := new(chunk)

	for {
		r, ok := recv(e.quit, e.receipts)
		if !ok {
			return
		}
		c.receipts = append(c.receipts, r.receipt)
		if !r.lastInBlock {
			continue
		}

		c.stateRoot = e.statedb.IntermediateRoot(true)
		if !send(e.quit, e.chunks, c) {
			return
		}
	}
}
