package sae

import (
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ava-labs/libevm/ethdb"
)

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

func (b *Block) writePostExecutionState(w ethdb.KeyValueWriter) error {
	return b.writeToKVStore(w, execResultsDBKey, b.execution.MarshalCanoto())
}

func (vm *VM) readPostExecutionState(blockNum uint64) (*executionResults, error) {
	buf, err := vm.db.Get(execResultsDBKey(blockNum))
	if err != nil {
		return nil, err
	}
	r := new(executionResults)
	if err := r.UnmarshalCanoto(buf); err != nil {
		return nil, err
	}
	return r, nil
}

/* ===== Last-settled block at chain height ===== */

func lastSettledDBKey(blockNum uint64) []byte {
	return blockNumDBKey("sae-last-settled-", blockNum)
}

func (b *Block) writeLastSettledNumber(w ethdb.KeyValueWriter) error {
	return b.writeToKVStore(w, lastSettledDBKey, b.lastSettled.Number().Bytes())
}

func (vm *VM) readLastSettledNumber(blockNum uint64) (uint64, error) {
	buf, err := vm.db.Get(lastSettledDBKey(blockNum))
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
