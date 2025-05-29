package sae

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/ethdb"
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
	return b.CurrentHeader()
}

func (b *ethAPIBackend) CurrentHeader() *types.Header {
	bb := b.vm.last.executed.Load()
	return types.CopyHeader(bb.Header())
}

func (b *ethAPIBackend) SuggestGasTipCap(context.Context) (*big.Int, error) {
	// TODO(arr4n) the API adds this value to the base fee from
	// [ethAPIBackend.CurrentHeader] to get the recommended gas price, so that
	// value needs to be subtracted from the tip of the queue and the difference
	// returned.
	return big.NewInt(10 * params.GWei), nil
}

type blockOrHeader interface {
	types.Block | types.Header
}

type blockOrHeaderReader[T blockOrHeader] func(ethdb.Reader, common.Hash, uint64) *T

var (
	_ blockOrHeaderReader[types.Block]  = rawdb.ReadBlock
	_ blockOrHeaderReader[types.Header] = rawdb.ReadHeader
)

func readBlockOrHeaderByNumber[T blockOrHeader](b *ethAPIBackend, blockNum rpc.BlockNumber, read blockOrHeaderReader[T]) (*T, error) {
	num, hash, err := b.resolveBlockNumber(blockNum)
	if err != nil {
		return nil, err
	}
	ret := read(b.vm.db, hash, uint64(num))
	if ret == nil {
		return nil, fmt.Errorf("read %T (%s): %w", ret, blockNum, database.ErrNotFound)
	}
	return ret, nil
}

func (b *ethAPIBackend) resolveBlockNumber(num rpc.BlockNumber) (uint64, common.Hash, error) {
	switch {
	case num == rpc.LatestBlockNumber:
		return b.blockNumAndHash(&b.vm.last.executed)
	case num == rpc.SafeBlockNumber:
		return b.blockNumAndHash(&b.vm.last.settled)
	case num < 0:
		// Other labelled blocks: pending, finalized, and future definitions.
		return 0, common.Hash{}, fmt.Errorf("%s block %w", num.String(), errUnsupported)
	}

	hash := rawdb.ReadCanonicalHash(b.vm.db, uint64(num))
	if hash == (common.Hash{}) {
		return 0, hash, fmt.Errorf("canonical hash for block %d: %w", num, database.ErrNotFound)
	}
	return uint64(num), hash, nil
}

// blockNumAndHash always returns a nil error; the signature is for convenience
// when used in [ethAPIBackend.resolveBlockNumber].
func (*ethAPIBackend) blockNumAndHash(block *atomic.Pointer[Block]) (uint64, common.Hash, error) {
	b := block.Load()
	return b.NumberU64(), b.Hash(), nil
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, num rpc.BlockNumber) (*types.Block, error) {
	return readBlockOrHeaderByNumber(b, num, rawdb.ReadBlock)
}

func (b *ethAPIBackend) HeaderByNumber(ctx context.Context, num rpc.BlockNumber) (*types.Header, error) {
	return readBlockOrHeaderByNumber(b, num, rawdb.ReadHeader)
}

func readBlockOrHeaderByHash[T blockOrHeader](b *ethAPIBackend, hash common.Hash, read blockOrHeaderReader[T]) (*T, error) {
	num, err := b.blockNumFromHash(hash)
	if err != nil {
		return nil, err
	}
	return readBlockOrHeaderByNumber(b, rpc.BlockNumber(num), read)
}

func (b *ethAPIBackend) blockNumFromHash(hash common.Hash) (uint64, error) {
	num := rawdb.ReadHeaderNumber(b.vm.db, hash)
	if num == nil {
		return 0, fmt.Errorf("read block number for %#x: %w", hash, database.ErrNotFound)
	}
	return *num, nil
}

func (b *ethAPIBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return readBlockOrHeaderByHash(b, hash, rawdb.ReadBlock)
}

func (b *ethAPIBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return readBlockOrHeaderByHash(b, hash, rawdb.ReadHeader)
}

func readBlockOrHeader[T blockOrHeader](
	ctx context.Context,
	numOrHash rpc.BlockNumberOrHash,
	byNum func(context.Context, rpc.BlockNumber) (*T, error),
	byHash func(context.Context, common.Hash) (*T, error),
) (*T, error) {
	if n, ok := numOrHash.Number(); ok {
		return byNum(ctx, n)
	}
	h, ok := numOrHash.Hash()
	if !ok {
		var zero T
		return nil, fmt.Errorf("neither number nor hash specified when fetching %T", zero)
	}
	return byHash(ctx, h)
}

func (b *ethAPIBackend) BlockByNumberOrHash(ctx context.Context, numOrHash rpc.BlockNumberOrHash) (*types.Block, error) {
	return readBlockOrHeader(ctx, numOrHash, b.BlockByNumber, b.BlockByHash)
}

func (b *ethAPIBackend) HeaderByNumberOrHash(ctx context.Context, numOrHash rpc.BlockNumberOrHash) (*types.Header, error) {
	return readBlockOrHeader(ctx, numOrHash, b.HeaderByNumber, b.HeaderByHash)
}

func (b *ethAPIBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	// TODO(arr4n): add an LRU to the VM for faster access to recently executed
	// txs and blocks.

	hdr, err := b.HeaderByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	num := hdr.Number.Uint64()
	if num > b.vm.last.executed.Load().NumberU64() {
		return nil, fmt.Errorf("block %s (%#x) not executed yet", hdr.Number.String(), hash)
	}

	return rawdb.ReadReceipts(
		b.vm.db,
		hash,
		num,
		// Time is used to construct a [types.Signer] in
		// [types.Receipts.DeriveFields] so it MUST be the block's time, not the
		// execution time of the receipts.
		hdr.Time,
		b.vm.exec.chainConfig,
	), nil
}

func (b *ethAPIBackend) GetTransaction(ctx context.Context, txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNum, index := rawdb.ReadTransaction(b.vm.db, txHash)
	if tx == nil {
		return false, nil, common.Hash{}, 0, 0, nil
	}
	return true, tx, blockHash, blockNum, index, nil
}

// StateAndHeaderByNumberOrHash does NOT return the consensus-agreed
// [types.Header], but one augmented to reflect the post-execution state of the
// requested block. Similarly, the returned [state.StateDB] is opened at the
// post-execution root of the block.
//
// This behaviour reflects the expected usage of this method, which is to query
// state (e.g. balances, nonces, storage) and to perform `eth_call` or
// `eth_estimateGas` operations. These operations are all performed by tooling
// that was built for a synchronous execution model, and the method's behaviour
// mimics such a setup.
func (b *ethAPIBackend) StateAndHeaderByNumberOrHash(ctx context.Context, numOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	h, err := b.HeaderByNumberOrHash(ctx, numOrHash)
	if err != nil {
		return nil, nil, err
	}

	// TODO(arr4n) use a last-synchronous block as the pivot point for async
	// execution; a genesis block suffices, but so too does a synchronous chain
	// being upgraded.
	const lastSynchronousBlockHeight = 0
	if num := h.Number.Uint64(); num > lastSynchronousBlockHeight {
		res, err := b.vm.readPostExecutionState(num)
		if err != nil {
			return nil, nil, err
		}
		h.Root = res.stateRootPost
	}

	db, err := state.New(h.Root, b.vm.exec.stateCache, nil)
	if err != nil {
		return nil, nil, err
	}
	return db, h, nil
}

func (b *ethAPIBackend) GetEVM(ctx context.Context, msg *core.Message, db *state.StateDB, hdr *types.Header, config *vm.Config, context *vm.BlockContext) *vm.EVM {
	txCtx := vm.TxContext{
		Origin:   msg.From,
		GasPrice: hdr.BaseFee,
	}
	if txCtx.GasPrice == nil {
		txCtx.GasPrice = big.NewInt(0)
	}
	return vm.NewEVM(*context, txCtx, db, b.vm.exec.chainConfig, *config)
}

func (*ethAPIBackend) RPCEVMTimeout() time.Duration {
	return 100 * time.Millisecond
}

func (*ethAPIBackend) RPCGasCap() uint64 {
	return uint64(10 * maxGasPerSecond)
}

func (b *ethAPIBackend) Engine() consensus.Engine {
	return engine{}
}

// GetTd is required by the API frontend for unmarshalling a [types.Block], but
// the result is never used so we return nil.
func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int { return nil }
