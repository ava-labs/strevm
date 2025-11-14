package sae

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"math"
	"math/big"
	"slices"
	"sort"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"go.uber.org/zap"
)

func (vm *VM) upgradeLastSynchronousBlock(lastSync LastSynchronousBlock) error {
	lastSyncNum := rawdb.ReadHeaderNumber(vm.db, lastSync.Hash)
	if lastSyncNum == nil {
		return fmt.Errorf("read number of last synchronous block (%#x): %w", lastSync.Hash, database.ErrNotFound)
	}
	ethBlock := rawdb.ReadBlock(vm.db, lastSync.Hash, *lastSyncNum)
	if ethBlock == nil {
		return fmt.Errorf("read last synchronous block (%#x): %w", lastSync.Hash, database.ErrNotFound)
	}

	s := &vm.last.synchronous
	s.height = *lastSyncNum
	s.time = ethBlock.Time()

	lastSyncNumBig := new(big.Int).SetUint64(*lastSyncNum)
	if rawdb.ReadHeadHeader(vm.db).Number.Cmp(lastSyncNumBig) > 0 {
		return nil
	}

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

	clock := gastime.New(block.Time(), lastSync.Target, lastSync.ExcessAfter)

	receipts := rawdb.ReadRawReceipts(vm.db, lastSync.Hash, block.Height())
	if err := block.MarkExecuted(vm.db, clock, block.Timestamp(), receipts, block.Block.Root(), vm.hooks); err != nil {
		return err
	}
	if err := block.WriteLastSettledNumber(vm.db); err != nil {
		return err
	}
	if err := block.MarkSettled(); err != nil {
		return err
	}

	if err := block.CheckInvariants(blocks.Settled); err != nil {
		return fmt.Errorf("upgrading last synchronous block: %v", err)
	}
	vm.logger().Info(
		"Last synchronous block before SAE",
		zap.Uint64("timestamp", block.Time()),
		zap.Stringer("hash", block.Hash()),
	)
	return nil
}

func (vm *VM) recoverFromDB(ctx context.Context, chainConfig *params.ChainConfig) (*dbRecovery, error) {
	rec, err := vm.newDBRecovery(chainConfig)
	if err != nil {
		return nil, err
	}
	lastCommitted := rec.blocks[rec.lastCommittedNum]

	vm.last.accepted.Store(lastCommitted)
	vm.last.executed.Store(lastCommitted)

	lastSettled := lastCommitted.LastSettled()
	if lastCommitted.Height() == vm.last.synchronous.height {
		lastSettled = lastCommitted
		if err := lastCommitted.MarkSettled(); err != nil {
			return nil, err
		}
	}
	vm.last.settled.Store(lastSettled)

	if err := vm.addBlocksToMemory(ctx, slices.Collect(maps.Values(rec.blocks))...); err != nil {
		return nil, err
	}
	return rec, nil
}

type dbRecovery struct {
	db          ethdb.Database
	chainConfig *params.ChainConfig
	logger      logging.Logger

	lastCommittedNum  uint64
	lastCommittedRoot common.Hash
	lastWithExecState uint64 // executed but state root not committed
	recover           []blockToRecover
	blocks            map[uint64]*blocks.Block
}

type blockToRecover struct {
	num  uint64
	hash common.Hash
}

func (vm *VM) newDBRecovery(chainConfig *params.ChainConfig) (*dbRecovery, error) {
	db := vm.db

	// Although this block has its post-execution results stored in the
	// database, we don't necessarily have the state root of those results
	// committed to disk. This is, however, a tight lower bound for the highest
	// canonical block number.
	lastExecutedNum := rawdb.ReadHeadHeader(db).Number.Uint64()

	// The number of unexecuted blocks can never be greater than that which
	// could fit in a queue, so it's safe to use [math.MaxUint64] as the upper
	// bound.
	unExecutedNums, _ /*hashes*/ := rawdb.ReadAllCanonicalHashes(db, lastExecutedNum, math.MaxUint64, math.MaxInt)
	var lastAcceptedNum uint64
	for _, n := range unExecutedNums {
		lastAcceptedNum = max(n, lastAcceptedNum)
	}

	lastCommittedAt := max(lastTrieDBCommittedAt(lastAcceptedNum), vm.last.synchronous.height)
	lastCommittedNum, err := blocks.ReadLastSettledNumber(db, lastCommittedAt)
	if err != nil {
		return nil, fmt.Errorf("read block number of last committed trie root: %v", err)
	}
	lastCommittedRoot, err := blocks.StateRootPostExecution(db, lastCommittedNum)

	// Although we only need to re-executed from `lastCommittedNum+1`, this
	// requires internal block invariants such as ancestry. We'll recover all of
	// these, but not necessarily re-execute all of them.
	from, err := blocks.ReadLastSettledNumber(db, lastCommittedNum)
	if err != nil {
		return nil, fmt.Errorf("read block number of last-settled of last-committed: %v", err)
	}

	var recover []blockToRecover
	nums, hashes := rawdb.ReadAllCanonicalHashes(db, from, math.MaxUint64, math.MaxInt)
	for i, n := range nums {
		recover = append(recover, blockToRecover{
			num:  n,
			hash: hashes[i],
		})
	}
	sort.Slice(recover, func(i, j int) bool {
		return recover[i].num < recover[j].num
	})

	byNum := make(map[uint64]*blocks.Block)
	for i := range nums {
		num := nums[i]
		if num > lastCommittedNum {
			break
		}
		b, err := recoverBlockFromDB(
			vm.db,
			num, hashes[i],
			true, // recover execution state
			byNum, chainConfig, vm.logger(),
		)
		if err != nil {
			return nil, err
		}
		byNum[num] = b
	}

	return &dbRecovery{
		db:                db,
		chainConfig:       chainConfig,
		logger:            vm.logger(),
		lastCommittedNum:  lastCommittedNum,
		lastCommittedRoot: lastCommittedRoot,
		lastWithExecState: lastExecutedNum,
		recover:           recover,
		blocks:            byNum,
	}, nil
}

func recoverBlockFromDB(
	db ethdb.Database,
	num uint64, hash common.Hash,
	recoverExecutionState bool,
	existing map[uint64]*blocks.Block,
	chainConfig *params.ChainConfig, logger logging.Logger,
) (*blocks.Block, error) {
	settled, err := blocks.ReadLastSettledNumber(db, num)
	if err != nil {
		return nil, err
	}

	ethB := rawdb.ReadBlock(db, hash, num)
	if ethB == nil {
		return nil, fmt.Errorf("rawdb.ReadBlock(%#x, %d) returned nil", hash, num)
	}
	b, err := blocks.New(ethB, existing[num-1], existing[settled], logger)
	if err != nil {
		return nil, err
	}
	if recoverExecutionState {
		if err := b.RestorePostExecutionStateAndReceipts(db, chainConfig); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (dbr *dbRecovery) blocksToReExecute() iter.Seq2[*blocks.Block, error] {
	var executeIdx int
	for i, b := range dbr.recover {
		if b.num == dbr.lastCommittedNum {
			executeIdx = i
			break
		}
	}

	return func(yield func(*blocks.Block, error) bool) {
		if executeIdx+1 == len(dbr.recover) {
			return
		}

		for _, rec := range dbr.recover[executeIdx+1:] {
			b, err := recoverBlockFromDB(
				dbr.db,
				rec.num, rec.hash,
				false, // recover execution state
				dbr.blocks, dbr.chainConfig, dbr.logger,
			)
			if !yield(b, err) {
				return
			}
			if err != nil {
				return
			}
			dbr.blocks[rec.num] = b

			// Pruning doesn't need to be perfect, and this may miss some blocks
			// that we recovered but aren't reexecuting. We only need to ensure
			// that the map doesn't OOM us when replaying 2^12 blocks.
			delete(dbr.blocks, b.LastSettled().Height()-1)
		}
	}
}

func (vm *VM) reexecuteBlocksAfterShutdown(ctx context.Context, rec *dbRecovery) error {
	for b, err := range rec.blocksToReExecute() {
		if err != nil {
			return err
		}
		if err := vm.addBlocksToMemory(ctx, b); err != nil {
			return err
		}

		var expectRoot *common.Hash
		if b.Height() <= rec.lastWithExecState {
			r, err := blocks.StateRootPostExecution(vm.db, b.Height())
			if err != nil {
				return err
			}
			expectRoot = &r
		}

		if err := vm.AcceptBlock(ctx, b); err != nil {
			return err
		}
		if err := b.WaitUntilExecuted(ctx); err != nil {
			return err
		}
		if r := b.PostExecutionStateRoot(); expectRoot != nil && r != *expectRoot {
			return fmt.Errorf("re-execution of block %d after shutdown resulted in root %#x when expecting %#x", b.Height(), r, *expectRoot)
		}

		vm.logger().Debug(
			"Re-executed block after shutdown",
			zap.Uint64("height", b.Height()),
		)
	}
	vm.preference.Store(vm.last.accepted.Load())
	return nil
}
