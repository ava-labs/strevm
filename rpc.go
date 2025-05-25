package sae

import (
	"context"
	"net/http"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rpc"
)

func (vm *VM) ethRPCHandler() http.Handler {
	s := rpc.NewServer()
	s.RegisterName("eth", ethRPC{vm: vm})
	return s
}

type ethRPC struct {
	vm *VM
}

func (rpc *ethRPC) SendRawTransaction(ctx context.Context, buf hexutil.Bytes) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(buf); err != nil {
		return common.Hash{}, err
	}

	select {
	case <-ctx.Done():
		return common.Hash{}, ctx.Err()
	case rpc.vm.newTxs <- tx:
		return tx.Hash(), nil
	}
}
