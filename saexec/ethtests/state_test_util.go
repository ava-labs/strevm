// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ethtests

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/libevm/triedb/hashdb"
	"github.com/ava-labs/libevm/triedb/pathdb"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/sha3"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/hook"
	"github.com/ava-labs/strevm/saetest"
)

// StateTest checks transaction processing without block context.
// See https://github.com/ethereum/EIPs/issues/176 for the test format specification.
type StateTest struct {
	json stJSON
}

// StateSubtest selects a specific configuration of a General State Test.
type StateSubtest struct {
	Fork  string
	Index int
}

func (t *StateTest) UnmarshalJSON(in []byte) error {
	return json.Unmarshal(in, &t.json)
}

type stJSON struct {
	Env  stEnv                    `json:"env"`
	Pre  types.GenesisAlloc       `json:"pre"`
	Tx   stTransaction            `json:"transaction"`
	Out  hexutil.Bytes            `json:"out"`
	Post map[string][]stPostState `json:"post"`
}

type stPostState struct {
	Root            common.UnprefixedHash `json:"hash"`
	Logs            common.UnprefixedHash `json:"logs"`
	TxBytes         hexutil.Bytes         `json:"txbytes"`
	ExpectException string                `json:"expectException"`
	Indexes         struct {
		Data  int `json:"data"`
		Gas   int `json:"gas"`
		Value int `json:"value"`
	}
}

//go:generate go run github.com/fjl/gencodec -type stEnv -field-override stEnvMarshaling -out gen_stenv.go

type stEnv struct {
	Coinbase      common.Address `json:"currentCoinbase"      gencodec:"required"`
	Difficulty    *big.Int       `json:"currentDifficulty"    gencodec:"optional"`
	Random        *big.Int       `json:"currentRandom"        gencodec:"optional"`
	GasLimit      uint64         `json:"currentGasLimit"      gencodec:"required"`
	Number        uint64         `json:"currentNumber"        gencodec:"required"`
	Timestamp     uint64         `json:"currentTimestamp"     gencodec:"required"`
	BaseFee       *big.Int       `json:"currentBaseFee"       gencodec:"optional"`
	ExcessBlobGas *uint64        `json:"currentExcessBlobGas" gencodec:"optional"`
}

type stEnvMarshaling struct {
	Coinbase      common.UnprefixedAddress
	Difficulty    *math.HexOrDecimal256
	Random        *math.HexOrDecimal256
	GasLimit      math.HexOrDecimal64
	Number        math.HexOrDecimal64
	Timestamp     math.HexOrDecimal64
	BaseFee       *math.HexOrDecimal256
	ExcessBlobGas *math.HexOrDecimal64
}

//go:generate go run github.com/fjl/gencodec -type stTransaction -field-override stTransactionMarshaling -out gen_sttransaction.go

type stTransaction struct {
	GasPrice             *big.Int            `json:"gasPrice"`
	MaxFeePerGas         *big.Int            `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *big.Int            `json:"maxPriorityFeePerGas"`
	Nonce                uint64              `json:"nonce"`
	To                   string              `json:"to"`
	Data                 []string            `json:"data"`
	AccessLists          []*types.AccessList `json:"accessLists,omitempty"`
	GasLimit             []uint64            `json:"gasLimit"`
	Value                []string            `json:"value"`
	PrivateKey           []byte              `json:"secretKey"`
	Sender               *common.Address     `json:"sender"`
	BlobVersionedHashes  []common.Hash       `json:"blobVersionedHashes,omitempty"`
	BlobGasFeeCap        *big.Int            `json:"maxFeePerBlobGas,omitempty"`
}

type stTransactionMarshaling struct {
	GasPrice             *math.HexOrDecimal256
	MaxFeePerGas         *math.HexOrDecimal256
	MaxPriorityFeePerGas *math.HexOrDecimal256
	Nonce                math.HexOrDecimal64
	GasLimit             []math.HexOrDecimal64
	PrivateKey           hexutil.Bytes
	BlobGasFeeCap        *math.HexOrDecimal256
}

// GetChainConfig takes a fork definition and returns a chain config.
// The fork definition can be
// - a plain forkname, e.g. `Byzantium`,
// - a fork basename, and a list of EIPs to enable; e.g. `Byzantium+1884+1283`.
func GetChainConfig(forkString string) (baseConfig *params.ChainConfig, eips []int, err error) {
	var (
		splitForks            = strings.Split(forkString, "+")
		ok                    bool
		baseName, eipsStrings = splitForks[0], splitForks[1:]
	)
	if baseConfig, ok = Forks[baseName]; !ok {
		return nil, nil, UnsupportedForkError{baseName}
	}
	for _, eip := range eipsStrings {
		if eipNum, err := strconv.Atoi(eip); err != nil {
			return nil, nil, fmt.Errorf("syntax error, invalid eip number %v", eipNum)
		} else {
			if !vm.ValidEip(eipNum) {
				return nil, nil, fmt.Errorf("syntax error, invalid eip number %v", eipNum)
			}
			eips = append(eips, eipNum)
		}
	}
	return baseConfig, eips, nil
}

// Subtests returns all valid subtests of the test.
func (t *StateTest) Subtests() []StateSubtest {
	var sub []StateSubtest
	for fork, pss := range t.json.Post {
		for i := range pss {
			sub = append(sub, StateSubtest{fork, i})
		}
	}
	return sub
}

// checkError checks if the error returned by the state transition matches any expected error.
// A failing expectation returns a wrapped version of the original error, if any,
// or a new error detailing the failing expectation.
// This function does not return or modify the original error, it only evaluates and returns expectations for the error.
func (t *StateTest) checkError(subtest StateSubtest, err error) error {
	expectedError := t.json.Post[subtest.Fork][subtest.Index].ExpectException
	if err == nil && expectedError == "" {
		return nil
	}
	if err == nil && expectedError != "" {
		return fmt.Errorf("expected error %q, got no error", expectedError)
	}
	if err != nil && expectedError == "" {
		return fmt.Errorf("unexpected error: %w", err)
	}
	if err != nil && expectedError != "" {
		// Ignore expected errors (TODO MariusVanDerWijden check error string)
		return nil
	}
	return nil
}

// Run executes a specific subtest and verifies the post-state and logs
func (t *StateTest) Run(tb testing.TB, subtest StateSubtest, vmconfig vm.Config, snapshotter bool, scheme string, postCheck func(err error, st *StateTestState)) (result error) {
	tb.Helper()
	st, root, receipts, err := t.RunWithSAE(tb, subtest, snapshotter, scheme)
	// Invoke the callback at the end of function for further analysis.
	defer func() {
		postCheck(result, &st)
	}()

	checkedErr := t.checkError(subtest, err)
	if checkedErr != nil {
		return checkedErr
	}
	// The error has been checked; if it was unexpected, it's already returned.
	if err != nil {
		// Here, an error exists but it was expected.
		// We do not check the post state or logs.
		return nil
	}
	post := t.json.Post[subtest.Fork][subtest.Index]
	// N.B: We need to do this in a two-step process, because the first Commit takes care
	// of self-destructs, and we need to touch the coinbase _after_ it has potentially self-destructed.
	if root != common.Hash(post.Root) {
		return fmt.Errorf("post state root mismatch: got %x, want %x", root, post.Root)
	}
	// Collect logs from receipts for hash comparison. Logs are transient data
	// stored in memory during execution, not persisted in the state trie.
	var logs []*types.Log
	for _, receipt := range receipts {
		logs = append(logs, receipt.Logs...)
	}
	if logsHash := rlpHash(logs); logsHash != common.Hash(post.Logs) {
		return fmt.Errorf("post state logs hash mismatch: got %x, want %x", logsHash, post.Logs)
	}
	st.StateDB, _ = state.New(root, st.StateDB.Database(), st.Snapshots)
	return nil
}

// RunWithSAE executes a specific subtest using the SAE executor and verifies the post-state and logs.
// This method uses the saexec.Executor to execute the transaction within a block, which tests the
// SAE execution path instead of direct EVM execution.
func (t *StateTest) RunWithSAE(tb testing.TB, subtest StateSubtest, snapshotter bool, scheme string) (st StateTestState, root common.Hash, receipts types.Receipts, err error) {
	tb.Helper()

	config, _, err := GetChainConfig(subtest.Fork)
	if err != nil {
		return st, root, nil, UnsupportedForkError{subtest.Fork}
	}

	// Configure trie database configuration
	tconf := &triedb.Config{
		Preimages: true,
	}
	if scheme == rawdb.PathScheme {
		tconf.PathDB = pathdb.Defaults
	} else {
		tconf.HashDB = hashdb.Defaults
	}

	// Configure snapshot configuration
	var snapshotConfig *snapshot.Config
	if snapshotter {
		snapshotConfig = &snapshot.Config{
			CacheSize:  1,
			Recovery:   false,
			NoBuild:    false,
			AsyncBuild: false,
		}
	}

	// CONTEXT(cey): This is different than upstream `genesis(config *params.ChainConfig)`. In upstream they use a single block
	// for tests, which is the genesis block but formed with the test data(env, pre, tx, out, post).
	//  In SAE we create a new block on top of the genesis block, and cannot modift things like number
	genesis := &core.Genesis{
		Config: config,
		Alloc:  t.json.Pre,
	}

	opts := []sutOption{
		withChainConfig(config),
		withTrieDBConfig(tconf),
		withSnapshotConfig(snapshotConfig),
		withGenesisSpec(genesis),
	}

	// Create hook factory for pre-state hooks
	hookFactory := func(chain *blockstest.ChainBuilder, db ethdb.Database, chainConfig *params.ChainConfig, logger *saetest.TBLogger) hook.Points {
		return newPreStateHooks(t.json.Pre)
	}

	ctx, sut := newSUT(tb, hookFactory, opts...)
	genesisBlock := sut.LastExecuted()

	// Get the post state configuration
	post := t.json.Post[subtest.Fork][subtest.Index]

	// Calculate base fee
	var baseFee *big.Int
	if config.IsLondon(new(big.Int)) {
		baseFee = t.json.Env.BaseFee
		if baseFee == nil {
			// Retesteth uses `0x10` for genesis baseFee. Therefore, it defaults to
			// parent - 2 : 0xa as the basefee for 'this' context.
			baseFee = big.NewInt(0x0a)
		}
	}

	// Convert transaction from state test format to signed transaction
	tx, err := t.json.Tx.toTransaction(post, baseFee, config)
	if err != nil {
		return st, root, nil, err
	}

	// Check blob gas limits
	if len(tx.BlobHashes())*params.BlobTxBlobGasPerBlob > params.MaxBlobGasPerBlock {
		return st, root, nil, errors.New("blob gas exceeds maximum")
	}

	// Try to recover tx with current signer
	if len(post.TxBytes) != 0 {
		var ttx types.Transaction
		err := ttx.UnmarshalBinary(post.TxBytes)
		if err != nil {
			return st, root, nil, err
		}
		if _, err := types.Sender(types.LatestSigner(config), &ttx); err != nil {
			return st, root, nil, err
		}
	}

	// Create a block containing the transaction
	parent := genesisBlock.EthBlock()
	ethBlock := blockstest.NewEthBlock(
		parent,
		types.Transactions{tx},
		blockstest.ModifyHeader(func(h *types.Header) {
			h.Time = t.json.Env.Timestamp
			h.GasLimit = t.json.Env.GasLimit
			h.Coinbase = t.json.Env.Coinbase
			if t.json.Env.Difficulty != nil {
				h.Difficulty = t.json.Env.Difficulty
			}
			h.BaseFee = baseFee
			if config.IsLondon(new(big.Int)) && t.json.Env.Random != nil {
				h.MixDigest = common.BigToHash(t.json.Env.Random)
				h.Difficulty = big.NewInt(0)
			}
			if config.IsCancun(new(big.Int), h.Time) && t.json.Env.ExcessBlobGas != nil {
				h.ExcessBlobGas = t.json.Env.ExcessBlobGas
			}
		}),
	)

	// Create SAE block
	saeBlock := blockstest.NewBlock(tb, ethBlock, genesisBlock, nil)

	// Set up the parent's gas time to match expected base fee if needed
	if baseFee != nil {
		target := genesisBlock.ExecutedByGasTime().Target()
		desiredExcessGas := desiredExcessForStateTest(baseFee, target)
		fakeParent := blockstest.NewBlock(tb, parent, nil, nil)
		require.NoError(tb, fakeParent.MarkExecuted(sut.DB, gastime.New(ethBlock.Time(), target, desiredExcessGas), time.Time{}, baseFee, nil, genesisBlock.PostExecutionStateRoot()))
		require.Equal(tb, baseFee.Uint64(), fakeParent.ExecutedByGasTime().BaseFee().Uint64())
		// Update the SAE block's parent reference
		saeBlock = blockstest.NewBlock(tb, ethBlock, fakeParent, nil)
	}

	// Insert block into chain and enqueue for execution
	require.NoError(tb, sut.Enqueue(ctx, saeBlock))

	// Wait for execution to complete. If WaitUntilExecuted returns an error,
	// it means either the context was cancelled or execution failed.
	execErr := saeBlock.WaitUntilExecuted(ctx)

	if execErr != nil {
		return st, root, nil, execErr
	}

	sdb, err := state.New(saeBlock.PostExecutionStateRoot(), sut.StateCache(), sut.Snapshots())
	require.NoErrorf(tb, err, "state.New(%T.PostExecutionStateRoot(), %T.StateCache(), nil)", saeBlock, sut)

	st = StateTestState{
		StateDB: sdb,
		// CONTEXT(cey): We intentionally don't set TrieDB or Snapshots here because the
		// SUT owns these resources and will close them in sut.Close(). Setting
		// them here would cause st.Close() to close them first, making
		// sut.Close() fail.
	}

	root = saeBlock.PostExecutionStateRoot()
	receipts = saeBlock.Receipts()

	return st, root, receipts, nil
}

// desiredExcessForStateTest calculates the desired excess gas to achieve a specific base fee.
// This is a helper function similar to desiredExcess in block_test_util.go but simplified for state tests.
func desiredExcessForStateTest(desiredBaseFee *big.Int, target gas.Gas) gas.Gas {
	if desiredBaseFee == nil || desiredBaseFee.Sign() == 0 {
		return 0
	}
	// Use a simple approximation: excess = target * ln(desiredPrice / basePrice)
	// For state tests, we'll use a binary search similar to block_test_util.go
	desiredPrice := gas.Price(desiredBaseFee.Uint64())
	return gas.Gas(sort.Search(math.MaxInt32, func(excessGuess int) bool {
		tm := gastime.New(0, target, gas.Gas(excessGuess))
		price := tm.Price()
		return price >= desiredPrice
	}))
}

func (t *StateTest) gasLimit(subtest StateSubtest) uint64 {
	return t.json.Tx.GasLimit[t.json.Post[subtest.Fork][subtest.Index].Indexes.Gas]
}

func (tx *stTransaction) toTransaction(ps stPostState, baseFee *big.Int, config *params.ChainConfig) (*types.Transaction, error) {
	var key *ecdsa.PrivateKey
	// Derive private key if present
	if len(tx.PrivateKey) > 0 {
		var err error
		key, err = crypto.ToECDSA(tx.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %v", err)
		}
	} else {
		// CONTEXT(cey): This is different than upstream `toMessage`
		// because we require a private key to sign the transaction.
		return nil, errors.New("private key required to sign transaction")
	}
	// Parse recipient if present.
	var to *common.Address
	if tx.To != "" {
		to = new(common.Address)
		if err := to.UnmarshalText([]byte(tx.To)); err != nil {
			return nil, fmt.Errorf("invalid to address: %v", err)
		}
	}

	// Get values specific to this post state.
	if ps.Indexes.Data >= len(tx.Data) {
		return nil, fmt.Errorf("tx data index %d out of bounds", ps.Indexes.Data)
	}
	if ps.Indexes.Value >= len(tx.Value) {
		return nil, fmt.Errorf("tx value index %d out of bounds", ps.Indexes.Value)
	}
	if ps.Indexes.Gas >= len(tx.GasLimit) {
		return nil, fmt.Errorf("tx gas limit index %d out of bounds", ps.Indexes.Gas)
	}
	dataHex := tx.Data[ps.Indexes.Data]
	valueHex := tx.Value[ps.Indexes.Value]
	gasLimit := tx.GasLimit[ps.Indexes.Gas]
	// Value, Data hex encoding is messy: https://github.com/ethereum/tests/issues/203
	value := new(big.Int)
	if valueHex != "0x" {
		v, ok := math.ParseBig256(valueHex)
		if !ok {
			return nil, fmt.Errorf("invalid tx value %q", valueHex)
		}
		value = v
	}
	data, err := hex.DecodeString(strings.TrimPrefix(dataHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid tx data %q", dataHex)
	}
	var accessList types.AccessList
	if tx.AccessLists != nil && ps.Indexes.Data < len(tx.AccessLists) && tx.AccessLists[ps.Indexes.Data] != nil {
		accessList = *tx.AccessLists[ps.Indexes.Data]
	}

	// Determine transaction type and create appropriate TxData
	var txData types.TxData
	signer := types.LatestSigner(config)

	if config.ChainID == nil {
		return nil, errors.New("chain ID required for transactions")
	}

	chainID := uint256.NewInt(0)
	chainID.SetFromBig(config.ChainID)

	valueU256 := uint256.NewInt(0)
	valueU256.SetFromBig(value)

	// If baseFee provided, set gasPrice to effectiveGasPrice.
	gasPrice := tx.GasPrice
	if baseFee != nil {
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = gasPrice
		}
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = new(big.Int)
		}
		if tx.MaxPriorityFeePerGas == nil {
			tx.MaxPriorityFeePerGas = tx.MaxFeePerGas
		}
		gasPrice = math.BigMin(new(big.Int).Add(tx.MaxPriorityFeePerGas, baseFee),
			tx.MaxFeePerGas)
	}
	if gasPrice == nil {
		return nil, errors.New("no gas price provided")
	}

	// Determine if this is a blob transaction
	switch {
	case len(tx.BlobVersionedHashes) > 0:
		var toAddr common.Address
		if to != nil {
			toAddr = *to
		}

		txData = &types.BlobTx{
			ChainID:    chainID,
			Nonce:      tx.Nonce,
			GasFeeCap:  uint256.MustFromBig(tx.MaxFeePerGas),
			GasTipCap:  uint256.MustFromBig(tx.MaxPriorityFeePerGas),
			Gas:        gasLimit,
			To:         toAddr,
			Value:      valueU256,
			Data:       data,
			AccessList: accessList,
			BlobFeeCap: uint256.MustFromBig(tx.BlobGasFeeCap),
			BlobHashes: tx.BlobVersionedHashes,
		}
	case tx.MaxFeePerGas != nil:
		txData = &types.DynamicFeeTx{
			ChainID:    new(big.Int).Set(config.ChainID),
			Nonce:      tx.Nonce,
			GasFeeCap:  tx.MaxFeePerGas,
			GasTipCap:  tx.MaxPriorityFeePerGas,
			Gas:        gasLimit,
			To:         to,
			Value:      value,
			Data:       data,
			AccessList: accessList,
		}
	case len(accessList) > 0:
		txData = &types.AccessListTx{
			ChainID:    new(big.Int).Set(config.ChainID),
			Nonce:      tx.Nonce,
			GasPrice:   new(big.Int).Set(tx.GasPrice),
			Gas:        gasLimit,
			To:         to,
			Value:      value,
			Data:       data,
			AccessList: accessList,
		}
	default:
		// Legacy transaction
		if tx.GasPrice == nil {
			return nil, errors.New("no gas price provided")
		}

		txData = &types.LegacyTx{
			Nonce:    tx.Nonce,
			GasPrice: new(big.Int).Set(tx.GasPrice),
			Gas:      gasLimit,
			To:       to,
			Value:    value,
			Data:     data,
		}
	}

	// Sign the transaction
	signedTx, err := types.SignNewTx(key, signer, txData)
	if err != nil {
		return nil, fmt.Errorf("failed to sign transaction: %v", err)
	}

	return signedTx, nil
}

func (tx *stTransaction) toMessage(ps stPostState, baseFee *big.Int) (*core.Message, error) {
	var from common.Address
	// If 'sender' field is present, use that
	if tx.Sender != nil {
		from = *tx.Sender
	} else if len(tx.PrivateKey) > 0 {
		// Derive sender from private key if needed.
		key, err := crypto.ToECDSA(tx.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %v", err)
		}
		from = crypto.PubkeyToAddress(key.PublicKey)
	}
	// Parse recipient if present.
	var to *common.Address
	if tx.To != "" {
		to = new(common.Address)
		if err := to.UnmarshalText([]byte(tx.To)); err != nil {
			return nil, fmt.Errorf("invalid to address: %v", err)
		}
	}

	// Get values specific to this post state.
	if ps.Indexes.Data >= len(tx.Data) {
		return nil, fmt.Errorf("tx data index %d out of bounds", ps.Indexes.Data)
	}
	if ps.Indexes.Value >= len(tx.Value) {
		return nil, fmt.Errorf("tx value index %d out of bounds", ps.Indexes.Value)
	}
	if ps.Indexes.Gas >= len(tx.GasLimit) {
		return nil, fmt.Errorf("tx gas limit index %d out of bounds", ps.Indexes.Gas)
	}
	dataHex := tx.Data[ps.Indexes.Data]
	valueHex := tx.Value[ps.Indexes.Value]
	gasLimit := tx.GasLimit[ps.Indexes.Gas]
	// Value, Data hex encoding is messy: https://github.com/ethereum/tests/issues/203
	value := new(big.Int)
	if valueHex != "0x" {
		v, ok := math.ParseBig256(valueHex)
		if !ok {
			return nil, fmt.Errorf("invalid tx value %q", valueHex)
		}
		value = v
	}
	data, err := hex.DecodeString(strings.TrimPrefix(dataHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid tx data %q", dataHex)
	}
	var accessList types.AccessList
	if tx.AccessLists != nil && ps.Indexes.Data < len(tx.AccessLists) && tx.AccessLists[ps.Indexes.Data] != nil {
		accessList = *tx.AccessLists[ps.Indexes.Data]
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	gasPrice := tx.GasPrice
	if baseFee != nil {
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = gasPrice
		}
		if tx.MaxFeePerGas == nil {
			tx.MaxFeePerGas = new(big.Int)
		}
		if tx.MaxPriorityFeePerGas == nil {
			tx.MaxPriorityFeePerGas = tx.MaxFeePerGas
		}
		gasPrice = math.BigMin(new(big.Int).Add(tx.MaxPriorityFeePerGas, baseFee),
			tx.MaxFeePerGas)
	}
	if gasPrice == nil {
		return nil, errors.New("no gas price provided")
	}

	msg := &core.Message{
		From:          from,
		To:            to,
		Nonce:         tx.Nonce,
		Value:         value,
		GasLimit:      gasLimit,
		GasPrice:      gasPrice,
		GasFeeCap:     tx.MaxFeePerGas,
		GasTipCap:     tx.MaxPriorityFeePerGas,
		Data:          data,
		AccessList:    accessList,
		BlobHashes:    tx.BlobVersionedHashes,
		BlobGasFeeCap: tx.BlobGasFeeCap,
	}
	return msg, nil
}

func rlpHash(x interface{}) (h common.Hash) {
	hw := sha3.NewLegacyKeccak256()
	rlp.Encode(hw, x)
	hw.Sum(h[:0])
	return h
}

func vmTestBlockHash(n uint64) common.Hash {
	return common.BytesToHash(crypto.Keccak256([]byte(big.NewInt(int64(n)).String())))
}

// StateTestState groups all the state database objects together for use in tests.
type StateTestState struct {
	StateDB   *state.StateDB
	TrieDB    *triedb.Database
	Snapshots *snapshot.Tree
}

// Close should be called when the state is no longer needed, ie. after running the test.
func (st *StateTestState) Close() {
	if st.TrieDB != nil {
		st.TrieDB.Close()
		st.TrieDB = nil
	}
	if st.Snapshots != nil {
		// Need to call Disable here to quit the snapshot generator goroutine.
		st.Snapshots.Disable()
		st.Snapshots.Release()
		st.Snapshots = nil
	}
}
