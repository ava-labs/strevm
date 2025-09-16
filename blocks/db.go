// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"slices"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/trie"
)

/* ===== Common =====*/

func blockNumDBKey(prefix string, blockNum uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte(prefix), blockNum)
}

func (b *Block) writeToKVStore(w ethdb.KeyValueWriter, key func(uint64) []byte, val []byte) error {
	return w.Put(key(b.NumberU64()), val)
}

/* ===== Post-execution state =====*/

func execResultsDBKey(blockNum uint64) []byte {
	return blockNumDBKey("sae-post-exec-", blockNum)
}

func (b *Block) writePostExecutionState(w ethdb.KeyValueWriter, e *executionResults) error {
	return b.writeToKVStore(w, execResultsDBKey, e.MarshalCanoto())
}

// RestorePostExecutionState restores b to the same post-execution state as when
// [Block.MarkExecuted] was called on it—this is only expected to be used after
// a restart. The receipts MUST match those originally passed to
// [Block.MarkExecuted] as they will be checked against the persisted Merkle
// root.
//
// This method is considered equivalent to a call to [Block.MarkExecuted] for
// the purposes of post-execution events (e.g. unblocking
// [Block.WaitUntilExecuted]) and artefacts (e.g. [Block.ExecutedByGasTime]).
// Similarly, it MUST NOT be called more than once, and usage of this and
// [Block.MarkExecuted] is mutually exclusive.
func (b *Block) RestorePostExecutionState(db ethdb.Database, receipts types.Receipts) error {
	e, err := readExecResults(db, b.NumberU64())
	if err != nil {
		return err
	}
	if argRoot := types.DeriveSha(receipts, trie.NewStackTrie(nil)); argRoot != e.receiptRoot {
		return fmt.Errorf(
			"restoring execution state of block %d: receipt-root mismatch (db = %v; arg = %v)",
			b.Height(), e.receiptRoot, argRoot,
		)
	}
	e.receipts = slices.Clone(receipts)
	return b.markExecuted(e)
}

// RestorePostExecutionStateAndReceipts is a convenience wrapper for calling
// [rawdb.ReadReceipts], the results of which are propagated to
// [Block.RestorePostExecutionState].
func (b *Block) RestorePostExecutionStateAndReceipts(db ethdb.Database, config *params.ChainConfig) error {
	rs := rawdb.ReadReceipts(db, b.Hash(), b.NumberU64(), b.BuildTime(), config)
	return b.RestorePostExecutionState(db, rs)
}

// StateRootPostExecution returns the state root passed to [Block.MarkExecuted],
// as persisted in the database. The [Block.PostExecutionStateRoot] method is
// the in-memory equivalent of this function.
func StateRootPostExecution(db ethdb.Database, blockNum uint64) (common.Hash, error) {
	e, err := readExecResults(db, blockNum)
	if err != nil {
		return common.Hash{}, err
	}
	return e.stateRootPost, nil
}

func readExecResults(db ethdb.Database, num uint64) (*executionResults, error) {
	buf, err := db.Get(execResultsDBKey(num))
	if err != nil {
		return nil, err
	}
	e := new(executionResults)
	if err := e.UnmarshalCanoto(buf); err != nil {
		return nil, err
	}
	return e, nil
}

/* ===== Last-settled block at chain height ===== */

func lastSettledDBKey(blockNum uint64) []byte {
	return blockNumDBKey("sae-last-settled-", blockNum)
}

// WriteLastSettledNumber writes, to w, the block height of the last-settled
// block of b (i.e. of [Block.LastSettled]).
func (b *Block) WriteLastSettledNumber(w ethdb.KeyValueWriter) error {
	return b.writeToKVStore(w, lastSettledDBKey, b.LastSettled().Number().Bytes())
}

// ReadLastSettledNumber is the counterpart of [Block.WriteLastSettledNumber],
// returning the height of the last-settled block of the block with the
// specified height.
func ReadLastSettledNumber(db ethdb.Database, blockNum uint64) (uint64, error) {
	buf, err := db.Get(lastSettledDBKey(blockNum))
	if err != nil {
		return 0, err
	}
	settled := new(big.Int).SetBytes(buf)
	if !settled.IsUint64() {
		return 0, fmt.Errorf("read non-uint64 last-settled block of block %d", blockNum)
	}
	if settled.Uint64() >= blockNum { // only a sense check
		return 0, fmt.Errorf("read last-settled block num %d of block %d", settled.Uint64(), blockNum)
	}
	return settled.Uint64(), nil
}
