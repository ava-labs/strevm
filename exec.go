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

	q       sink.Mutex[[]*Block]
	pending chan sig

	// executeScratchSpace MUST NOT be accessed by any methods other than
	// [executor.init] and [executor.execute].
	executeScratchSpace executeScratchSpace
}

func (e *executor) init() error {
	db := state.NewDatabase(rawdb.NewMemoryDatabase())
	statedb, err := state.New(types.EmptyRootHash, db, nil)
	if err != nil {
		return err
	}
	x := &e.executeScratchSpace
	x.statedb = statedb
	x.curr = new(chunk)
	return nil
}

func (e *executor) start() {
	e.q = sink.NewMutex[[]*Block](nil)
	e.pending = make(chan sig, 1)
	e.spawn(e.enqueueAccepted)
	e.spawn(e.clearQueue)

	<-e.quit
	e.spawned.Wait()
	close(e.chunks)
	close(e.pending)
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
			e.logger().Fatal(
				"Executing enqueued block",
				zap.Error(err),
				zap.Uint64("height", b.Height()),
				zap.Any("hash", b.b.Hash()),
			)
			return
		}
	}
}

type executeScratchSpace struct {
	statedb   *state.StateDB
	curr      *chunk
	gasPool   core.GasPool
	gasConfig gas.Config
	excess    gas.Gas
}

type chunk struct {
	stateRoot common.Hash
	receipts  []*types.Receipt
	usedGas   uint64
}

func (e *executor) execute(b *Block) error {
	x := &e.executeScratchSpace
	x.gasPool.SetGas(math.MaxUint64)

	hdr := types.CopyHeader(b.b.Header())
	hdr.BaseFee = new(big.Int)
	maxChunkSize := x.gasConfig.MaxPerSecond
	_ = maxChunkSize

	txs := b.b.Transactions()
	x.curr.receipts = make([]*types.Receipt, len(txs))
	for ti, tx := range txs {
		price := gas.CalculatePrice(params.GWei, x.excess, x.gasConfig.ExcessConversionConstant)
		hdr.BaseFee.SetUint64(uint64(price))

		x.statedb.SetTxContext(tx.Hash(), ti)

		receipt, err := core.ApplyTransaction(
			params.TestChainConfig,
			e.chain,
			&hdr.Coinbase,
			&x.gasPool,
			x.statedb,
			hdr,
			tx,
			&x.curr.usedGas,
			vm.Config{},
		)
		if err != nil {
			return err
		}

		x.curr.receipts[ti] = receipt
		x.excess += gas.Gas(receipt.GasUsed / 2)
	}

	x.curr.stateRoot = x.statedb.IntermediateRoot(true)
	if !send(e.quit, e.chunks, x.curr) {
		return context.Canceled
	}
	return nil
}
