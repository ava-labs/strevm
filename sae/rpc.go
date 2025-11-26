// Copyright (C) ((20\d\d\-2025)|(2025)), Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"fmt"

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

	txAPI := ethapi.NewTransactionAPI(b, new(ethapi.AddrLocker))
	if err := s.RegisterName("eth", txAPI); err != nil {
		return nil, fmt.Errorf("%T.RegisterName(%q, %T): %v", s, "eth", txAPI, err)
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
