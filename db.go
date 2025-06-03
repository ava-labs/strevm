package sae

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"go.uber.org/zap"
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

func (vm *VM) upgradeLastSynchronousBlock(hash common.Hash) error {
	lastSyncNum := rawdb.ReadHeaderNumber(vm.db, hash)
	if lastSyncNum == nil {
		return fmt.Errorf("read number of last synchronous block (%#x): %w", hash, database.ErrNotFound)
	}
	block := vm.newBlock(rawdb.ReadBlock(vm.db, hash, *lastSyncNum))
	if block.Block == nil {
		return fmt.Errorf("read last synchronous block (%#x): %w", hash, database.ErrNotFound)
	}
	vm.last.synchronousTime = block.Time()

	block.execution = &executionResults{
		by: gasClock{
			time: block.Time(),
		},
		receipts:      nil, // nil to avoid async re-settlement
		gasUsed:       gas.Gas(block.GasUsed()),
		receiptRoot:   block.ReceiptHash(),
		stateRootPost: block.Root(),
	}
	block.executed.Store(true)

	// The last synchronous block is, by definition, already settled, so the
	// `parent` and `lastSettled` pointers MUST be nil (see the invariant
	// documented in [Block]). We do, however, need to write the last-settled
	// block number to the database; the deferred resetting to nil is defensive,
	// in case this method is refactored to return `block`.
	block.lastSettled = block
	defer func() { block.lastSettled = nil }()

	batch := vm.db.NewBatch()
	if err := block.writePostExecutionState(batch); err != nil {
		return err
	}
	if err := block.writeLastSettledNumber(batch); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return err
	}

	vm.logger().Info(
		"Last synchronous block before SAE",
		zap.Uint64("timestamp", block.Time()),
		zap.Stringer("hash", block.Hash()),
	)
	return nil
}

func (vm *VM) recoverFromDB(ctx context.Context, chainConfig *params.ChainConfig) error {
	db := vm.db

	lastExecutedHeader := rawdb.ReadHeadHeader(db)
	lastExecutedHash := lastExecutedHeader.Hash()
	lastExecutedNum := lastExecutedHeader.Number.Uint64()

	lastSettledHash := rawdb.ReadFinalizedBlockHash(db)
	lastSettledNum := rawdb.ReadHeaderNumber(db, lastSettledHash)
	if lastSettledNum == nil {
		return fmt.Errorf("database in corrupt state: rawdb.ReadHeaderNumber(rawdb.ReadFinalizedBlockHash() = %#x) returned nil", lastSettledHash)
	}

	// Although we won't need all of these blocks, the last-settled of the
	// last-settled block is a guaranteed and reasonable lower bound on the
	// blocks to recover.
	from, err := vm.readLastSettledNumber(*lastSettledNum)
	if err != nil {
		return err
	}

	// In [VM.AcceptBlock] we prune all blocks before the last-settled one so
	// that's all we need to recover.
	nums, hashes := rawdb.ReadAllCanonicalHashes(vm.db, from, math.MaxUint64, math.MaxInt)

	// [rawdb.ReadAllCanonicalHashes] does not state that it returns blocks in
	// increasing numerical order. It might, but it doesn't say so on the tin.
	lastAcceptedNum := *lastSettledNum
	lastAcceptedHash := lastSettledHash
	for i, num := range nums {
		if num <= lastAcceptedNum {
			continue
		}
		lastAcceptedNum = num
		lastAcceptedHash = hashes[i]
	}

	return vm.blocks.Replace(ctx, func(blockMap) (blockMap, error) {
		byID := make(blockMap)
		byNum := make(map[uint64]*Block)

		for i, num := range nums {
			hash := hashes[i]
			b := vm.newBlock(rawdb.ReadBlock(db, hash, num))
			if b.Block == nil {
				return nil, fmt.Errorf("rawdb.ReadBlock(%#x, %d) returned nil", hash, num)
			}
			byID[b.Hash()] = b
			byNum[num] = b

			if num <= lastExecutedNum {
				state, err := vm.readPostExecutionState(num)
				if err != nil {
					return nil, fmt.Errorf("recover post-execution state of block %d: %w", num, err)
				}
				state.receipts = rawdb.ReadReceipts(db, b.Hash(), b.NumberU64(), b.Time(), chainConfig)
				b.execution = state
				b.executed.Store(true)
			}

			if b.Hash() == lastExecutedHash {
				vm.last.executed.Store(b)
			}
			if b.Hash() == lastSettledHash {
				vm.last.settled.Store(b)
			}
			if b.Hash() == lastAcceptedHash {
				vm.last.accepted.Store(b)
			}
		}
		vm.preference.Store(vm.last.accepted.Load())

		for _, b := range byID {
			num := b.NumberU64()
			if num <= *lastSettledNum {
				// Once a block is settled, its `parent` and `lastSettled`
				// pointers are cleared to allow for GC, so we don't reinstate
				// them here.
				continue
			}

			b.parent = byNum[num-1]
			s, err := vm.readLastSettledNumber(num)
			if err != nil {
				return nil, err
			}
			b.lastSettled = byNum[s]
		}

		for _, num := range nums {
			b := byNum[num]
			// These were only needed as the last-settled pointers of blocks
			// that themselves are yet to be settled.
			if num < *lastSettledNum {
				delete(byID, b.Hash())
			}
		}

		return byID, nil
	})
}
