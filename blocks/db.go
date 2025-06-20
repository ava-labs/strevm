package blocks

import (
	"encoding/binary"
	"fmt"
	"math/big"

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
// [Block.MarkExecuted] was called on it. This is only expected to be used after
// a restart.
//
// The receipts MUST match those originally passed to [Block.MarkExecuted] as
// they will be checked against the persisted Merkle root.
func (b *Block) RestorePostExecutionState(db ethdb.Database, receipts types.Receipts) error {
	e, err := readExecResults(db, b.NumberU64())
	if err != nil {
		return err
	}
	if e.receiptRoot != types.DeriveSha(receipts, trie.NewStackTrie(nil)) {
		return fmt.Errorf("restoring execution state of block %d: receipt-root mismatch", b.Height())
	}
	return b.markExecuted(e)
}

// RestorePostExecutionStateAndReceipts is a convenience wrapper for calling
// [rawdb.ReadReceipts], the results of which are propagated to
// [Block.RestorePostExecutionState].
func (b *Block) RestorePostExecutionStateAndReceipts(db ethdb.Database, config *params.ChainConfig) error {
	rs := rawdb.ReadReceipts(db, b.Hash(), b.NumberU64(), b.Time(), config)
	return b.RestorePostExecutionState(db, rs)
}

// StateRootPostExecution returns the state root passed to [Block.MarkExecuted]
// and persisted with a call to [Block.WritePostExecutionState].
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

// WriteLastSettled number writes, to w, the block height of the last-settled
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
	if settled.Uint64() > blockNum {
		return 0, fmt.Errorf("read last-settled block num %d of block %d", settled.Uint64(), blockNum)
	}
	return settled.Uint64(), nil
}
