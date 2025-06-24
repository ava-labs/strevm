// Package hook defines points in a block's lifecycle at which common or
// user-injected behaviour needs to be performed. Functions in this package
// SHOULD be called by all code dealing with a block at the respective point in
// its lifecycle, be that during validation, execution, or otherwise.
package hook

import (
	"context"
	"iter"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/strevm/gastime"
	"github.com/holiman/uint256"
)

type ConstructBlock func(
	ctx context.Context,
	blockContext *block.Context,
	header *types.Header,
	parent *types.Header,
	ancestors iter.Seq[*types.Block],
	state State,
	txs []*types.Transaction,
	receipts []*types.Receipt,
) (*types.Block, error)

type Account struct {
	Nonce  uint64
	Amount uint256.Int
}

type Op struct {
	// Gas is the amount of gas consumed by this operation
	Gas gas.Gas
	// GasPrice is the largest gas price this operation is willing to spend
	GasPrice uint256.Int
	// From specifies the set of accounts and the authorization of funds to be
	// removed from the accounts.
	From map[common.Address]Account
	// To specifies the amount to increase account balances by. These funds are
	// not necessarily tied to the funds consumed in the From field. The sum of
	// the To amounts may even exceed the sum of the From amounts.
	To map[common.Address]uint256.Int
}

type State interface {
	Apply(o Op) error
}

// Points define user-injected hook points.
type Points interface {
	GasTarget(parent *types.Block) gas.Gas

	// Called during build
	ConstructBlock(
		ctx context.Context,
		blockContext *block.Context, // May be nil
		header *types.Header,
		parent *types.Header,
		ancestors iter.Seq[*types.Block],
		state State,
		txs []*types.Transaction,
		receipts []*types.Receipt,
	) (*types.Block, error)

	// Called during verify
	ConstructBlockFromBlock(ctx context.Context, block *types.Block) (ConstructBlock, error)

	// Called during historical worst case tracking + execution
	ExtraBlockOperations(ctx context.Context, block *types.Block) ([]Op, error)

	// Called after the block has been executed by the node.
	BlockExecuted(ctx context.Context, block *types.Block) error
}

// BeforeBlock is intended to be called before processing a block, with the gas
// target sourced from [Points].
func BeforeBlock(clock *gastime.Time, block *types.Header, target gas.Gas) {
	clock.FastForwardTo(block.Time)
	clock.SetTarget(target)
}

// AfterBlock is intended to be called after processing a block, with the gas
// sourced from [types.Block.GasUsed] or equivalent.
func AfterBlock(clock *gastime.Time, used gas.Gas) {
	clock.Tick(used)
}
