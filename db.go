package sae

import (
	"context"
	"fmt"
	"math"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"go.uber.org/zap"
)

func (vm *VM) upgradeLastSynchronousBlock(hash common.Hash) error {
	lastSyncNum := rawdb.ReadHeaderNumber(vm.db, hash)
	if lastSyncNum == nil {
		return fmt.Errorf("read number of last synchronous block (%#x): %w", hash, database.ErrNotFound)
	}
	ethBlock := rawdb.ReadBlock(vm.db, hash, *lastSyncNum)
	if ethBlock == nil {
		return fmt.Errorf("read last synchronous block (%#x): %w", hash, database.ErrNotFound)
	}
	vm.last.synchronousTime = ethBlock.Time()

	// The last synchronous block is, by definition, already settled (an
	// invariant of synchronous execution) so we use a self reference to record
	// as the last-settled block number. There is no need to pass a parent
	// because [block.Block] clears its ancestry once marked as settled (below).
	selfSettle, err := blocks.New(ethBlock, nil, nil, vm.logger())
	if err != nil {
		return err
	}
	block, err := blocks.New(ethBlock, nil, selfSettle, vm.logger())
	if err != nil {
		return err
	}

	if err := block.MarkExecuted(
		gastime.New(
			block.Time(),
			// TODO(arr4n) get the gas target and post-execution excess of the
			// genesis block.
			1e6, 0,
		),
		block.Timestamp(),
		nil, // avoid re-settlement of receipts,
		block.Root(),
	); err != nil {
		return err
	}

	batch := vm.db.NewBatch()
	if err := block.WritePostExecutionState(batch); err != nil {
		return err
	}
	if err := block.WriteLastSettledNumber(batch); err != nil {
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

	// Although the block isn't returned, do this defensively in case of a
	// future refactor.
	block.MarkSettled()
	if err := block.CheckInvariants(true, true); err != nil {
		return fmt.Errorf("upgrading last synchronous block: %v", err)
	}
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
	from, err := blocks.ReadLastSettledNumber(vm.db, *lastSettledNum)
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
			ethB := rawdb.ReadBlock(db, hash, num)
			if ethB == nil {
				return nil, fmt.Errorf("rawdb.ReadBlock(%#x, %d) returned nil", hash, num)
			}
			// We don't know the parent and last-settled blocks yet so will
			// populate them in the next loop with a call to
			// [block.Block.CopyInternalsFrom].
			b, err := vm.newBlock(ethB, nil, nil)
			if err != nil {
				return nil, err
			}

			byID[b.Hash()] = b
			byNum[num] = b

			if num <= lastExecutedNum {
				receipts := rawdb.ReadReceipts(db, b.Hash(), b.NumberU64(), b.Time(), chainConfig)
				if err := b.RestorePostExecutionState(vm.db, receipts); err != nil {
					return nil, err
				}
			}
			if num <= *lastSettledNum {
				b.MarkSettled()
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

			s, err := blocks.ReadLastSettledNumber(vm.db, num)
			if err != nil {
				return nil, err
			}
			parent := byNum[num-1]
			withAncestors, err := blocks.New(b.Block, parent, byNum[s], vm.logger())
			if err != nil {
				return nil, err
			}
			if err := b.CopyAncestorsFrom(withAncestors); err != nil {
				return nil, err
			}
		}

		for _, num := range nums {
			b := byNum[num]
			// These were only needed as the last-settled pointers of blocks
			// that themselves are yet to be settled.
			if num < *lastSettledNum {
				delete(byID, b.Hash())
			}
		}

		for _, b := range byID {
			num := b.Height()
			if err := b.CheckInvariants(num <= lastExecutedNum, num <= *lastSettledNum); err != nil {
				return nil, err
			}
		}
		return byID, nil
	})
}
