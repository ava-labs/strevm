package sae

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"
)

func (vm *VM) ethRPCHandler() http.Handler {
	b := &ethAPIBackend{vm: vm}
	s := rpc.NewServer()

	s.RegisterName("eth", ethapi.NewBlockChainAPI(b))
	s.RegisterName("eth", ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker)))
	return s
}

type ethAPIBackend struct {
	ethapi.Backend
	vm *VM
}

func (b *ethAPIBackend) ChainConfig() *params.ChainConfig {
	return b.vm.exec.chainConfig
}

func (b *ethAPIBackend) RPCTxFeeCap() float64 {
	return math.MaxFloat64
}

func (b *ethAPIBackend) UnprotectedAllowed() bool {
	// Allow without Chain-ID replay protection (EIP-155)
	return true
}

func (b *ethAPIBackend) SendTx(ctx context.Context, tx *types.Transaction) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.vm.newTxs <- tx:
		return nil
	}
}

func (b *ethAPIBackend) CurrentBlock() *types.Header {
	bb := b.vm.last.executed.Load()
	return bb.Header()
}

func (b *ethAPIBackend) BlockByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	if n, ok := blockNrOrHash.Number(); ok {
		return b.resolveBlockNumber(ctx, n)
	}

	h, ok := blockNrOrHash.Hash()
	if !ok {
		return nil, errors.New("neither block number nor hash specified")
	}
	_ = h
	return nil, errors.New("block by hash unimplemented")
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, num rpc.BlockNumber) (*types.Block, error) {
	bl, err := b.resolveBlockNumber(ctx, num)
	if err != nil {
		return nil, err
	}
	return bl, nil
}

func (b *ethAPIBackend) HeaderByNumber(ctx context.Context, num rpc.BlockNumber) (*types.Header, error) {
	bl, err := b.resolveBlockNumber(ctx, num)
	if err != nil {
		return nil, err
	}
	return bl.Header(), nil
}

func (b *ethAPIBackend) resolveBlockNumber(ctx context.Context, num rpc.BlockNumber) (*types.Block, error) {
	var ptr *atomic.Pointer[Block]

	switch num {
	// Named blocks are resolved relative to their execution status:
	// * pending execution	=> accepted
	// * latest execution	=> executed
	// * safe/finalized		=> settled
	case rpc.PendingBlockNumber:
		ptr = &b.vm.last.accepted
	case rpc.LatestBlockNumber:
		ptr = &b.vm.last.executed
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		ptr = &b.vm.last.settled

	default: // includes [rpc.EarliestBlockNumber] == 0
		n := uint64(num)
		hash := rawdb.ReadCanonicalHash(b.vm.db, n)
		b := rawdb.ReadBlock(b.vm.db, hash, n)
		if b == nil {
			return nil, database.ErrNotFound
		}
		return b, nil
	}

	return ptr.Load().Block, nil
}

func (b *ethAPIBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	// TODO(arr4n): add an LRU to the VM for faster access to recently executed
	// txs and blocks.

	num := rawdb.ReadHeaderNumber(b.vm.db, hash)
	if num == nil {
		return nil, fmt.Errorf("block number for %#x: %w", hash, database.ErrNotFound)
	}
	if *num > b.vm.last.executed.Load().NumberU64() {
		return nil, fmt.Errorf("block %d (%#x) not executed yet", *num, hash)
	}
	return rawdb.ReadReceipts(
		b.vm.db,
		hash,
		*num,
		// Time is used to construct a [types.Signer] in
		// [types.Receipts.DeriveFields] so it MUST be the block's time, not the
		// execution time of the receipts.
		rawdb.ReadHeader(b.vm.db, hash, *num).Time,
		b.vm.exec.chainConfig,
	), nil
}

// GetTd is required by the API frontend for unmarshalling a [types.Block], but
// the result is never used so we return nil.
func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int { return nil }
