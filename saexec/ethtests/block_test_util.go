// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
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

// Package ethtests implements execution of Ethereum JSON tests.
package ethtests

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/common/hexutil"
	"github.com/ava-labs/libevm/common/math"
	"github.com/ava-labs/libevm/consensus/beacon"
	"github.com/ava-labs/libevm/consensus/ethash"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/rlp"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/libevm/triedb/hashdb"
	"github.com/ava-labs/libevm/triedb/pathdb"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/gastime"
)

// A BlockTest checks handling of entire blocks.
type BlockTest struct {
	json btJSON
}

// UnmarshalJSON implements json.Unmarshaler interface.
func (t *BlockTest) UnmarshalJSON(in []byte) error {
	return json.Unmarshal(in, &t.json)
}

type btJSON struct {
	Blocks     []btBlock             `json:"blocks"`
	Genesis    btHeader              `json:"genesisBlockHeader"`
	Pre        types.GenesisAlloc    `json:"pre"`
	Post       types.GenesisAlloc    `json:"postState"`
	BestBlock  common.UnprefixedHash `json:"lastblockhash"`
	Network    string                `json:"network"`
	SealEngine string                `json:"sealEngine"`
}

type btBlock struct {
	BlockHeader     *btHeader
	ExpectException string
	Rlp             string
	UncleHeaders    []*btHeader
}

//go:generate go run github.com/fjl/gencodec -type btHeader -field-override btHeaderMarshaling -out gen_btheader.go

type btHeader struct {
	Bloom                 types.Bloom
	Coinbase              common.Address
	MixHash               common.Hash
	Nonce                 types.BlockNonce
	Number                *big.Int
	Hash                  common.Hash
	ParentHash            common.Hash
	ReceiptTrie           common.Hash
	StateRoot             common.Hash
	TransactionsTrie      common.Hash
	UncleHash             common.Hash
	ExtraData             []byte
	Difficulty            *big.Int
	GasLimit              uint64
	GasUsed               uint64
	Timestamp             uint64
	BaseFeePerGas         *big.Int
	WithdrawalsRoot       *common.Hash
	BlobGasUsed           *uint64
	ExcessBlobGas         *uint64
	ParentBeaconBlockRoot *common.Hash
}

type btHeaderMarshaling struct {
	ExtraData     hexutil.Bytes
	Number        *math.HexOrDecimal256
	Difficulty    *math.HexOrDecimal256
	GasLimit      math.HexOrDecimal64
	GasUsed       math.HexOrDecimal64
	Timestamp     math.HexOrDecimal64
	BaseFeePerGas *math.HexOrDecimal256
	BlobGasUsed   *math.HexOrDecimal64
	ExcessBlobGas *math.HexOrDecimal64
}

// Run runs the block test.
//
// snapshotter: whether to use snapshots.
// scheme: the scheme to use for the trie database. (rawdb.PathScheme or rawdb.HashScheme)
// tracer: the tracer to use for the execution.
// postCheck: the post-check function to run after the execution.
//
// Returns the result of the execution.
func (t *BlockTest) Run(tb testing.TB, snapshotter bool, scheme string, tracer vm.EVMLogger, postCheck func(error, *SUT)) (result error) {
	tb.Helper()
	config, ok := Forks[t.json.Network]
	if !ok {
		return UnsupportedForkError{t.json.Network}
	}
	opts := []sutOption{withChainConfig(config)}

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
	opts = append(opts, withTrieDBConfig(tconf))
	if snapshotter {
		snapshotConfig := snapshot.Config{
			CacheSize:  1,
			AsyncBuild: false,
		}
		opts = append(opts, withSnapshotConfig(&snapshotConfig))
	}
	// Commit genesis state
	gspec := t.genesis(config)
	opts = append(opts, withGenesisSpec(gspec))

	// Create SUT with consensus hooks factory
	engine := beacon.New(ethash.NewFaker())
	ctx, sut := newSUT(tb, NewTestConsensusHooksFactory(engine), opts...)
	gblock := sut.LastExecuted()
	require.Equal(tb, gblock.Hash(), t.json.Genesis.Hash)
	require.Equal(tb, gblock.PostExecutionStateRoot(), t.json.Genesis.StateRoot)
	require.Equal(tb, gblock.Header().Root, t.json.Genesis.StateRoot)

	validBlocks, err := t.insertBlocks(tb, ctx, &sut)
	if err != nil {
		return err
	}
	// Import succeeded: regardless of whether the _test_ succeeds or not, schedule
	// the post-check to run
	if postCheck != nil {
		defer postCheck(result, &sut)
	}
	last := sut.Chain.Last()
	lastHash := last.Hash()
	if common.Hash(t.json.BestBlock) != lastHash {
		return fmt.Errorf("last block hash validation mismatch: want: %x, have: %x", t.json.BestBlock, lastHash)
	}

	sdb, err := state.New(last.PostExecutionStateRoot(), sut.StateCache(), nil)
	require.NoErrorf(tb, err, "state.New(%T.PostExecutionStateRoot(), %T.StateCache(), nil)", last, sut)
	if err = t.validatePostState(sdb); err != nil {
		return fmt.Errorf("post state validation failed: %v", err)
	}
	// Cross-check the snapshot-to-hash against the trie hash
	if snapshotter {
		snaps := sut.Snapshots()
		require.NoErrorf(tb, err, "snapshot.New(..., %T.PostExecutionStateRoot())", sut)
		if err := snaps.Verify(last.PostExecutionStateRoot()); err != nil {
			return err
		}
	}
	return t.validateImportedHeaders(sut.Chain, validBlocks)
}

func (t *BlockTest) genesis(config *params.ChainConfig) *core.Genesis {
	return &core.Genesis{
		Config:        config,
		Nonce:         t.json.Genesis.Nonce.Uint64(),
		Timestamp:     t.json.Genesis.Timestamp,
		ParentHash:    t.json.Genesis.ParentHash,
		ExtraData:     t.json.Genesis.ExtraData,
		GasLimit:      t.json.Genesis.GasLimit,
		GasUsed:       t.json.Genesis.GasUsed,
		Difficulty:    t.json.Genesis.Difficulty,
		Mixhash:       t.json.Genesis.MixHash,
		Coinbase:      t.json.Genesis.Coinbase,
		Alloc:         t.json.Pre,
		BaseFee:       t.json.Genesis.BaseFeePerGas,
		BlobGasUsed:   t.json.Genesis.BlobGasUsed,
		ExcessBlobGas: t.json.Genesis.ExcessBlobGas,
	}
}

/*
See https://github.com/ethereum/tests/wiki/Blockchain-Tests-II

	Whether a block is valid or not is a bit subtle, it's defined by presence of
	blockHeader, transactions and uncleHeaders fields. If they are missing, the block is
	invalid and we must verify that we do not accept it.

	Since some tests mix valid and invalid blocks we need to check this for every block.

	If a block is invalid it does not necessarily fail the test, if it's invalidness is
	expected we are expected to ignore it and continue processing and then validate the
	post state.
*/
func (t *BlockTest) insertBlocks(tb testing.TB, ctx context.Context, sut *SUT) ([]btBlock, error) {
	tb.Helper()
	validBlocks := make([]btBlock, 0)
	blocks := make([]*types.Block, 0)
	// insert the test blocks, which will execute all transactions
	for bi, b := range t.json.Blocks {
		cb, err := b.decode()
		if err != nil {
			if b.BlockHeader == nil {
				tb.Log("Block decoding failed", "index", bi, "err", err)
				continue // OK - block is supposed to be invalid, continue with next block
			} else {
				return nil, fmt.Errorf("block RLP decoding failed when expected to succeed: %v", err)
			}
		}

		if b.BlockHeader == nil {
			// TODO(cey): We don't have any insertion rules at the moment, so skip them.
			tb.Logf("skipping checking the invalid test block insertion (index %d) with expected exception: %v, because we don't have any insertion rules", bi, b.ExpectException)
			tb.SkipNow()
			if data, err := json.MarshalIndent(cb.Header(), "", "  "); err == nil {
				fmt.Fprintf(os.Stderr, "block (index %d) insertion should have failed due to: %v:\n%v\n",
					bi, b.ExpectException, string(data))
			}
			return nil, fmt.Errorf("block (index %d) insertion should have failed due to: %v",
				bi, b.ExpectException)
		}

		// validate RLP decoding by checking all values against test file JSON
		if err = validateHeader(b.BlockHeader, cb.Header()); err != nil {
			return nil, fmt.Errorf("deserialised block header validation failed: %v", err)
		}
		validBlocks = append(validBlocks, b)
		blocks = append(blocks, cb)
	}

	// Insert the blocks into the chain
	insertWithHeaderBaseFee(tb, sut, blocks)
	return validBlocks, nil
}

func insertWithHeaderBaseFee(tb testing.TB, sut *SUT, bs types.Blocks) {
	tb.Helper()
	for _, b := range bs {
		parent := sut.Chain.Last()
		baseFee := b.BaseFee()
		// TODO(cey): This is a hack to set the base fee to the block header base fee.
		// Instead we should properly modify the test fixtures to apply expected base fee from the gasclock.
		if baseFee != nil {
			parent = fakeParentWithBaseFee(tb, sut.DB, parent, b.Time(), baseFee)
		}
		wb := blockstest.NewBlock(tb, b, parent, nil)
		sut.Chain.Insert(wb)
		require.NoError(tb, sut.Enqueue(tb.Context(), wb))
		require.NoError(tb, wb.WaitUntilExecuted(tb.Context()))
	}
}

// desiredExcess calculates the optimal excess gas given the desired base fee.
// This could be solved directly by calculating D * ln(desiredPrice / P)
// using floating point math. However, it introduces inaccuracies. So, we
// use a binary search to find the closest integer solution.
func desiredExcess(desiredBaseFee *big.Int, target gas.Gas) gas.Gas {
	if desiredBaseFee == nil || desiredBaseFee.Sign() == 0 {
		return 0
	}
	desiredPrice := gas.Price(desiredBaseFee.Uint64())
	return gas.Gas(sort.Search(math.MaxInt32, func(excessGuess int) bool { //nolint:gosec // Known to not overflow
		tm := gastime.New(0, target, gas.Gas(excessGuess)) //nolint:gosec // Known to not overflow
		price := tm.Price()
		return price >= desiredPrice
	}))
}

// fakeParentWithBaseFee creates a fake parent block with the desired base fee.
// This is used in tests to override the gas clock to produce the expected base fee.
func fakeParentWithBaseFee(tb testing.TB, db ethdb.Database, parent *blocks.Block, blockTime uint64, desiredBaseFee *big.Int) *blocks.Block {
	tb.Helper()
	target := parent.ExecutedByGasTime().Target()
	excess := desiredExcess(desiredBaseFee, target)
	var grandParent *blocks.Block
	if parent.NumberU64() != 0 {
		grandParent = parent.ParentBlock()
	}
	fakeParent := blockstest.NewBlock(tb, parent.EthBlock(), grandParent, nil)
	require.NoError(tb, fakeParent.MarkExecuted(db, gastime.New(blockTime, target, excess), time.Time{}, desiredBaseFee, nil, parent.PostExecutionStateRoot()))
	require.Equal(tb, desiredBaseFee.Uint64(), fakeParent.ExecutedByGasTime().BaseFee().Uint64())
	return fakeParent
}

func validateHeader(h *btHeader, h2 *types.Header) error {
	if h.Bloom != h2.Bloom {
		return fmt.Errorf("bloom: want: %x have: %x", h.Bloom, h2.Bloom)
	}
	if h.Coinbase != h2.Coinbase {
		return fmt.Errorf("coinbase: want: %x have: %x", h.Coinbase, h2.Coinbase)
	}
	if h.MixHash != h2.MixDigest {
		return fmt.Errorf("MixHash: want: %x have: %x", h.MixHash, h2.MixDigest)
	}
	if h.Nonce != h2.Nonce {
		return fmt.Errorf("nonce: want: %x have: %x", h.Nonce, h2.Nonce)
	}
	if h.Number.Cmp(h2.Number) != 0 {
		return fmt.Errorf("number: want: %v have: %v", h.Number, h2.Number)
	}
	if h.ParentHash != h2.ParentHash {
		return fmt.Errorf("parent hash: want: %x have: %x", h.ParentHash, h2.ParentHash)
	}
	if h.ReceiptTrie != h2.ReceiptHash {
		return fmt.Errorf("receipt hash: want: %x have: %x", h.ReceiptTrie, h2.ReceiptHash)
	}
	if h.TransactionsTrie != h2.TxHash {
		return fmt.Errorf("tx hash: want: %x have: %x", h.TransactionsTrie, h2.TxHash)
	}
	if h.StateRoot != h2.Root {
		return fmt.Errorf("state hash: want: %x have: %x", h.StateRoot, h2.Root)
	}
	if h.UncleHash != h2.UncleHash {
		return fmt.Errorf("uncle hash: want: %x have: %x", h.UncleHash, h2.UncleHash)
	}
	if !bytes.Equal(h.ExtraData, h2.Extra) {
		return fmt.Errorf("extra data: want: %x have: %x", h.ExtraData, h2.Extra)
	}
	if h.Difficulty.Cmp(h2.Difficulty) != 0 {
		return fmt.Errorf("difficulty: want: %v have: %v", h.Difficulty, h2.Difficulty)
	}
	if h.GasLimit != h2.GasLimit {
		return fmt.Errorf("gasLimit: want: %d have: %d", h.GasLimit, h2.GasLimit)
	}
	if h.GasUsed != h2.GasUsed {
		return fmt.Errorf("gasUsed: want: %d have: %d", h.GasUsed, h2.GasUsed)
	}
	if h.Timestamp != h2.Time {
		return fmt.Errorf("timestamp: want: %v have: %v", h.Timestamp, h2.Time)
	}
	if !reflect.DeepEqual(h.BaseFeePerGas, h2.BaseFee) {
		return fmt.Errorf("baseFeePerGas: want: %v have: %v", h.BaseFeePerGas, h2.BaseFee)
	}
	if !reflect.DeepEqual(h.WithdrawalsRoot, h2.WithdrawalsHash) {
		return fmt.Errorf("withdrawalsRoot: want: %v have: %v", h.WithdrawalsRoot, h2.WithdrawalsHash)
	}
	if !reflect.DeepEqual(h.BlobGasUsed, h2.BlobGasUsed) {
		return fmt.Errorf("blobGasUsed: want: %v have: %v", h.BlobGasUsed, h2.BlobGasUsed)
	}
	if !reflect.DeepEqual(h.ExcessBlobGas, h2.ExcessBlobGas) {
		return fmt.Errorf("excessBlobGas: want: %v have: %v", h.ExcessBlobGas, h2.ExcessBlobGas)
	}
	if !reflect.DeepEqual(h.ParentBeaconBlockRoot, h2.ParentBeaconRoot) {
		return fmt.Errorf("parentBeaconBlockRoot: want: %v have: %v", h.ParentBeaconBlockRoot, h2.ParentBeaconRoot)
	}
	return nil
}

func (t *BlockTest) validatePostState(statedb *state.StateDB) error {
	// validate post state accounts in test file against what we have in state db
	for addr, acct := range t.json.Post {
		// address is indirectly verified by the other fields, as it's the db key
		code2 := statedb.GetCode(addr)
		balance2 := statedb.GetBalance(addr).ToBig()
		nonce2 := statedb.GetNonce(addr)
		if !bytes.Equal(code2, acct.Code) {
			return fmt.Errorf("account code mismatch for addr: %s want: %v have: %s", addr, acct.Code, hex.EncodeToString(code2))
		}
		if balance2.Cmp(acct.Balance) != 0 {
			return fmt.Errorf("account balance mismatch for addr: %s, want: %d, have: %d", addr, acct.Balance, balance2)
		}
		if nonce2 != acct.Nonce {
			return fmt.Errorf("account nonce mismatch for addr: %s want: %d have: %d", addr, acct.Nonce, nonce2)
		}
		for k, v := range acct.Storage {
			v2 := statedb.GetState(addr, k)
			if v2 != v {
				return fmt.Errorf("account storage mismatch for addr: %s, slot: %x, want: %x, have: %x", addr, k, v, v2)
			}
		}
	}
	return nil
}

func (t *BlockTest) validateImportedHeaders(cb *blockstest.ChainBuilder, validBlocks []btBlock) error {
	// to get constant lookup when verifying block headers by hash (some tests have many blocks)
	bmap := make(map[common.Hash]btBlock, len(t.json.Blocks))
	for _, b := range validBlocks {
		bmap[b.BlockHeader.Hash] = b
	}
	// iterate over blocks backwards from HEAD and validate imported
	// headers vs test file. some tests have reorgs, and we import
	// block-by-block, so we can only validate imported headers after
	// all blocks have been processed by BlockChain, as they may not
	// be part of the longest chain until last block is imported.
	// Iterate backwards from the last block number to genesis (block 0 excluded)
	lastBlock := cb.Last()
	lastNumber := lastBlock.NumberU64()
	for blockNumber := lastNumber; blockNumber > 0; blockNumber-- {
		blockHash, ok := cb.GetHashAtHeight(blockNumber)
		if !ok {
			return fmt.Errorf("block at height %d not found", blockNumber)
		}
		b, ok := cb.GetBlock(blockHash, blockNumber)
		if !ok {
			return fmt.Errorf("block %x at height %d not found", blockHash, blockNumber)
		}
		if err := validateHeader(bmap[b.Hash()].BlockHeader, b.Header()); err != nil {
			return fmt.Errorf("imported block header validation failed: %v", err)
		}
	}
	return nil
}

func (bb *btBlock) decode() (*types.Block, error) {
	data, err := hexutil.Decode(bb.Rlp)
	if err != nil {
		return nil, err
	}
	var b types.Block
	err = rlp.DecodeBytes(data, &b)
	return &b, err
}
