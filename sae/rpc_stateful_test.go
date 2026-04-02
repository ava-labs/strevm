// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"math/big"
	"testing"
	"time"

	"github.com/arr4n/shed/testerr"
	"github.com/ava-labs/avalanchego/utils"
	ethereum "github.com/ava-labs/libevm"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/eth/tracers/logger"
	"github.com/ava-labs/libevm/ethclient/gethclient"
	"github.com/ava-labs/libevm/rpc"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/cmputils"
	saeparams "github.com/ava-labs/strevm/params"
	"github.com/ava-labs/strevm/saetest/escrow"
)

func TestDebugTrace(t *testing.T) {
	ctx, sut := newSUT(t, 1)

	escrowAddr := crypto.CreateAddress(sut.wallet.Addresses()[0], 0)
	recv := common.Address{'r', 'e', 'c', 'v'}
	const depositVal = 42

	sign := sut.wallet.SetNonceAndSign
	deployTx := sign(t, 0, &types.LegacyTx{
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CreationCode(),
	})
	depositTx := sign(t, 0, &types.LegacyTx{
		To:       &escrowAddr,
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CallDataToDeposit(recv),
		Value:    big.NewInt(depositVal),
	})

	b := sut.runConsensusLoop(t, deployTx, depositTx)
	require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	require.Lenf(t, b.Receipts(), 2, "%T.Receipts()", b)
	for _, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Status", r)
	}

	// Specifying the entire trace would be excessive and uninformative so we
	// select a precise location of an event associated with the deposit()
	// function on the contract.
	const logPC = 185
	require.Equal(t, vm.LOG1, vm.OpCode(escrow.ByteCode()[logPC]), "Bad test setup; program counter LOG1 for `emit Deposit()`")
	ignore := cmp.Options{
		cmpopts.IgnoreSliceElements(func(r logger.StructLogRes) bool {
			return r.Pc != logPC || r.Op != vm.LOG1.String()
		}),
		// Any precise amount of gas left at the time of OpCode execution would
		// be copy-pasted from the test output.
		cmpopts.IgnoreFields(logger.StructLogRes{}, "Gas", "GasCost"),
	}

	want := []struct {
		TxHash common.Hash             `json:"txHash"`
		Result *logger.ExecutionResult `json:"result"`
		Error  string                  `json:"error"`
	}{
		{
			TxHash: deployTx.Hash(),
			Result: &logger.ExecutionResult{
				Gas:         b.Receipts()[0].GasUsed,
				ReturnValue: common.Bytes2Hex(escrow.ByteCode()),
				StructLogs:  []logger.StructLogRes{},
			},
		},
		{
			TxHash: depositTx.Hash(),
			Result: &logger.ExecutionResult{
				Gas: b.Receipts()[1].GasUsed,
				StructLogs: []logger.StructLogRes{{
					Pc:    logPC,
					Op:    vm.LOG1.String(),
					Depth: 1,
					Stack: utils.PointerTo([]string{
						escrow.DepositEvent(recv, uint256.NewInt(depositVal)).Topics[0].String(),
						"0x40", "0x80", // arbitrary memory locations selected by Solidity
					}),
				}},
			},
		},
	}

	tests := []rpcTest{
		{
			method:       "debug_traceBlockByNumber",
			args:         []any{hexutil.Uint64(b.NumberU64())},
			want:         want,
			extraCmpOpts: ignore,
		},
		{
			method:       "debug_traceBlockByNumber",
			args:         []any{rpc.LatestBlockNumber},
			want:         want,
			extraCmpOpts: ignore,
		},
		{
			method:       "debug_traceBlockByHash",
			args:         []any{b.Hash()},
			want:         want,
			extraCmpOpts: ignore,
		},
		{
			method:  "debug_traceTransaction",
			args:    []any{common.Hash{}},
			wantErr: testerr.Contains("not found"),
		},
	}

	for _, tx := range want {
		tests = append(tests, rpcTest{
			method:       "debug_traceTransaction",
			args:         []any{tx.TxHash},
			want:         *tx.Result,
			extraCmpOpts: ignore,
		})
	}

	sut.testRPC(ctx, t, tests...)
}

func TestStatefulRPCs(t *testing.T) {
	opt, vmTime := withVMTime(t, time.Unix(saeparams.TauSeconds, 0))
	ctx, sut := newSUT(t, 1, opt)

	deploy := &types.LegacyTx{
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CreationCode(),
	}

	escrowAddr := crypto.CreateAddress(sut.wallet.Addresses()[0], 0)
	recv := common.Address{'r', 'e', 'c', 'v'}
	const depositVal = 42
	deposit := &types.LegacyTx{
		To:       &escrowAddr,
		Gas:      1e6,
		GasPrice: big.NewInt(1),
		Data:     escrow.CallDataToDeposit(recv),
		Value:    big.NewInt(depositVal),
	}

	sign := sut.wallet.SetNonceAndSign
	b := sut.runConsensusLoop(t, sign(t, 0, deploy), sign(t, 0, deposit))
	require.Len(t, b.Transactions(), 2, "tx count")
	require.NoErrorf(t, b.WaitUntilExecuted(ctx), "%T.WaitUntilExecuted()", b)
	for _, r := range b.Receipts() {
		require.Equalf(t, types.ReceiptStatusSuccessful, r.Status, "%T.Status", r)
	}
	deploymentGasUsed := b.Receipts()[0].GasUsed

	vmTime.advanceToSettle(ctx, t, b)
	for range 2 {
		bb := sut.runConsensusLoop(t)
		vmTime.advanceToSettle(ctx, t, bb)
	}
	_, ok := sut.rawVM.consensusCritical.Load(b.Hash())
	require.Falsef(t, ok, "%T[%#x] still in VM memory", b, b.Hash())

	balanceCallMsg := ethereum.CallMsg{
		From: sut.wallet.Addresses()[0],
		To:   &escrowAddr,
		Data: escrow.CallDataForBalance(recv),
	}

	tests := []struct {
		name string
		num  rpc.BlockNumber
	}{
		{
			name: "block_in_memory",
			num:  rpc.LatestBlockNumber,
		},
		{
			name: "block_on_disk",
			num:  rpc.BlockNumber(b.Number().Int64()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blockNum := big.NewInt(int64(tt.num))

			// Some methods aren't simple to call directly so we use the
			// [ethclient.Client], but prefer [SUT.testRPC] where possible.
			t.Run("eth_call", func(t *testing.T) {
				got, err := sut.CallContract(ctx, balanceCallMsg, blockNum)
				require.NoErrorf(t, err, "%T.CallContract()", sut.Client)
				want := uint256.NewInt(depositVal).PaddedBytes(32)
				assert.Equal(t, want, got, "Return value from balance()")
			})

			t.Run("eth_getProof", func(t *testing.T) {
				got, err := sut.GetProof(ctx, escrowAddr, nil, blockNum)
				require.NoErrorf(t, err, "%T.GetProof()", sut.gethClient.Client)

				opts := cmp.Options{
					cmputils.BigInts(),
					cmpopts.EquateEmpty(),
					cmpopts.IgnoreFields(
						gethclient.AccountResult{},
						// As we didn't implement the API endpoint itself we're
						// only testing plumbing. Although we could demonstrate
						// correctness of these fields it would be overkill.
						"AccountProof",
						"StorageHash",
					),
				}
				want := &gethclient.AccountResult{
					Address:  escrowAddr,
					Balance:  big.NewInt(depositVal),
					Nonce:    1,
					CodeHash: crypto.Keccak256Hash(escrow.ByteCode()),
				}
				if diff := cmp.Diff(want, got, opts); diff != "" {
					t.Errorf("Diff (-want +got):\n%s", diff)
				}
			})

			sut.testRPC(ctx, t, []rpcTest{
				{
					method: "eth_getCode",
					args:   []any{escrowAddr, tt.num},
					want:   hexutil.Bytes(escrow.ByteCode()),
				},
				{
					method: "eth_getBalance",
					args:   []any{escrowAddr, tt.num},
					want:   hexBigU(depositVal),
				},
				{
					method: "eth_getStorageAt",
					args: []any{
						escrowAddr,
						escrow.BalanceStorageSlot(recv),
						tt.num,
					},
					want: hexutil.Bytes(uint256.NewInt(depositVal).PaddedBytes(32)),
				},
			}...)
		})
	}

	t.Run("eth_estimateGas", func(t *testing.T) {
		got, err := sut.EstimateGas(ctx, ethereum.CallMsg{Data: escrow.CreationCode()})
		require.NoError(t, err)
		assert.InEpsilon(t, deploymentGasUsed, got, 0.015)
	})

	// Always runs against the latest block.
	t.Run("eth_createAccessList", func(t *testing.T) {
		got, _, _, err := sut.CreateAccessList(ctx, balanceCallMsg)
		require.NoErrorf(t, err, "%T.CreateAccessList()", sut.gethClient.Client)
		want := &types.AccessList{{
			Address:     escrowAddr,
			StorageKeys: []common.Hash{escrow.BalanceStorageSlot(recv)},
		}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Diff (-want +got):\n%s", diff)
		}
	})
}
