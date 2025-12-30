// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/ethapi"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rpc"

	"github.com/ava-labs/strevm/txgossip"
)

func (vm *VM) ethRPCServer() (*rpc.Server, error) {
	b := &ethAPIBackend{
		vm:  vm,
		Set: vm.mempool,
	}
	s := rpc.NewServer()

	for _, ethAPI := range []any{
		ethapi.NewBlockChainAPI(b),
		ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker)),
	} {
		if err := s.RegisterName("eth", ethAPI); err != nil {
			return nil, fmt.Errorf("%T.RegisterName(%q, %T): %v", s, "eth", ethAPI, err)
		}
	}
	return s, nil
}

type ethAPIBackend struct {
	vm             *VM
	ethapi.Backend // TODO(arr4n) remove in favour of `var _ ethapi.Backend = (*ethAPIBackend)(nil)`
	*txgossip.Set
}

func (b *ethAPIBackend) ChainConfig() *params.ChainConfig {
	return b.vm.exec.ChainConfig()
}

func (b *ethAPIBackend) RPCTxFeeCap() float64 {
	return 0 // TODO(arr4n)
}

func (b *ethAPIBackend) UnprotectedAllowed() bool {
	return false
}

func (b *ethAPIBackend) CurrentBlock() *types.Header {
	return types.CopyHeader(b.vm.exec.LastExecuted().Header())
}

func (b *ethAPIBackend) GetTd(context.Context, common.Hash) *big.Int {
	return big.NewInt(0) // TODO(arr4n)
}

func (b *ethAPIBackend) BlockByNumber(ctx context.Context, n rpc.BlockNumber) (*types.Block, error) {
	num, err := b.resolveBlockNumber(n)
	if err != nil {
		return nil, err
	}
	return rawdb.ReadBlock(
		b.vm.db,
		rawdb.ReadCanonicalHash(b.vm.db, num),
		num,
	), nil
}

func (b *ethAPIBackend) resolveBlockNumber(bn rpc.BlockNumber) (uint64, error) {
	head := b.vm.last.accepted.Load().Height()

	switch bn {
	case rpc.PendingBlockNumber: // i.e. pending execution
		return head, nil
	case rpc.LatestBlockNumber:
		return b.vm.exec.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return b.vm.last.settled.Load().Height(), nil
	}

	if bn < 0 {
		// Any future definitions should be added above.
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > head {
		return 0, fmt.Errorf("block %d not accepted yet", n)
	}
	return n, nil
}
