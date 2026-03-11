// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package blocks

import (
	"errors"
	"fmt"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/rpc"
)

type (
	Chain interface {
		Consensus
		Frontier
		DB() ethdb.Database
	}

	Consensus interface {
		BlockInConsensus(common.Hash) (*Block, bool)
	}

	Frontier interface {
		AcceptanceFrontier
		ExecutionFrontier
		SettlementFrontier
	}

	AcceptanceFrontier interface {
		LastAccepted() *Block
	}
	ExecutionFrontier interface {
		LastExecuted() *Block
	}
	SettlementFrontier interface {
		LastSettled() *Block
	}
)

var (
	ErrNotFound               = errors.New("block not found")
	ErrFutureBlockNotResolved = fmt.Errorf("%w: not accepted yet", ErrNotFound)
	ErrNonCanonicalBlock      = fmt.Errorf("%w: non-canonical block", ErrNotFound)

	ErrNeitherNumberNorHash = fmt.Errorf("%T carrying neither number nor hash", rpc.BlockNumberOrHash{})
	ErrBothNumberAndHash    = fmt.Errorf("%T carrying both number and hash", rpc.BlockNumberOrHash{})
)

func ResolveRPCNumber(f Frontier, bn rpc.BlockNumber) (uint64, error) {
	tip := f.LastAccepted().Height()

	switch bn {
	case rpc.PendingBlockNumber:
		return tip, nil
	case rpc.LatestBlockNumber:
		return f.LastExecuted().Height(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		return f.LastSettled().Height(), nil
	}

	if bn < 0 {
		return 0, fmt.Errorf("%s block unsupported", bn.String())
	}
	n := uint64(bn) //nolint:gosec // Non-negative check performed above
	if n > tip {
		return 0, fmt.Errorf("%w: block %d", ErrFutureBlockNotResolved, n)
	}
	return n, nil
}

func ResolveBlockNumberOrHash(c Chain, numOrHash rpc.BlockNumberOrHash) (uint64, common.Hash, error) {
	rpcNum, isNum := numOrHash.Number()
	hash, isHash := numOrHash.Hash()

	switch {
	case isNum && isHash:
		return 0, common.Hash{}, ErrBothNumberAndHash

	case isNum:
		num, err := ResolveRPCNumber(c, rpcNum)
		if err != nil {
			return 0, common.Hash{}, err
		}

		hash := rawdb.ReadCanonicalHash(c.DB(), num)
		if hash == (common.Hash{}) {
			return 0, common.Hash{}, fmt.Errorf("block %d not found", num)
		}
		return num, hash, nil

	case isHash:
		if bl, ok := c.BlockInConsensus(hash); ok {
			n := bl.NumberU64()
			if numOrHash.RequireCanonical && hash != rawdb.ReadCanonicalHash(c.DB(), n) {
				return 0, common.Hash{}, ErrNonCanonicalBlock
			}
			return n, hash, nil
		}

		numPtr := rawdb.ReadHeaderNumber(c.DB(), hash)
		if numPtr == nil {
			return 0, common.Hash{}, fmt.Errorf("block %#x not found", hash)
		}
		// We only write canonical blocks to the database so there's no need to
		// perform a check.
		return *numPtr, hash, nil

	default:
		return 0, common.Hash{}, ErrNeitherNumberNorHash
	}
}

type (
	Reader[T any]        func(db ethdb.Reader, hash common.Hash, num uint64) *T
	ReaderWithErr[T any] func(db ethdb.Reader, hash common.Hash, num uint64) (*T, error)
	Extractor[T any]     func(*Block) *T
)

func (r Reader[T]) WithNilErr() ReaderWithErr[T] {
	return func(db ethdb.Reader, h common.Hash, n uint64) (*T, error) {
		return r(db, h, n), nil
	}
}

func FromNumber[T any](c Chain, n rpc.BlockNumber, fromDB ReaderWithErr[T]) (*T, error) {
	num, err := ResolveRPCNumber(c, n)
	if err != nil {
		return nil, err
	}
	return fromDB(c.DB(), rawdb.ReadCanonicalHash(c.DB(), num), num)
}

// FromHash returns `fromMem(b)` if a block with the specified hash is in the
// VM's memory, otherwise it returns `fromDB()` i.f.f. the block was previously
// accepted. If `fromDB()` is called then the block is guaranteed to exist if
// read with [rawdb] functions.
//
// A hash that is in neither of the VM's memory nor the database will result in
// a return of `(nil, errWhenNotFound)` to allow for usage with the [rawdb]
// pattern of returning `(nil, nil)`.
func FromHash[T any](c Chain, hash common.Hash, fromConsensus Extractor[T], fromDB ReaderWithErr[T]) (*T, error) {
	if blk, ok := c.BlockInConsensus(hash); ok {
		return fromConsensus(blk), nil
	}
	num := rawdb.ReadHeaderNumber(c.DB(), hash)
	if num == nil {
		return nil, ErrNotFound
	}
	return fromDB(c.DB(), hash, *num)
}

func FromNumberOrHash[T any](c Chain, blockNrOrHash rpc.BlockNumberOrHash, fromConsensus Extractor[T], fromDB ReaderWithErr[T]) (*T, error) {
	n, hash, err := ResolveBlockNumberOrHash(c, blockNrOrHash)
	if err != nil {
		return nil, err
	}
	if blk, ok := c.BlockInConsensus(hash); ok {
		return fromConsensus(blk), nil
	}
	return fromDB(c.DB(), hash, n)
}

func FromNumberAndHash[T any](c Chain, hash common.Hash, num rpc.BlockNumber, fromConsensus Extractor[T], fromDB ReaderWithErr[T]) (*T, error) {
	if hash == (common.Hash{}) {
		return nil, errors.New("empty block hash")
	}
	n, err := ResolveRPCNumber(c, num)
	if err != nil {
		return nil, err
	}

	if b, ok := c.BlockInConsensus(hash); ok {
		if b.NumberU64() != n {
			return nil, fmt.Errorf("found block number %d for hash %#x, expected %d", b.NumberU64(), hash, num)
		}
		return fromConsensus(b), nil
	}
	return fromDB(c.DB(), hash, n)
}
