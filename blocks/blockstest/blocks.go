// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package blockstest provides test helpers for constructing [Streaming
// Asynchronous Execution] (SAE) blocks.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package blockstest

import (
	"math"
	"math/big"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/saetest"
)

// An EthBlockOption configures the default block properties created by
// [NewEthBlock].
type EthBlockOption = options.Option[ethBlockProperties]

// NewEthBlock constructs a raw Ethereum block with the given arguments.
func NewEthBlock(parent *types.Block, txs types.Transactions, opts ...EthBlockOption) *types.Block {
	props := &ethBlockProperties{
		header: &types.Header{
			Number:        new(big.Int).Add(parent.Number(), big.NewInt(1)),
			ParentHash:    parent.Hash(),
			BaseFee:       big.NewInt(0),
			ExcessBlobGas: new(uint64),
		},
	}
	props = options.ApplyTo(props, opts...)
	return types.NewBlock(props.header, txs, nil, props.receipts, saetest.TrieHasher())
}

type ethBlockProperties struct {
	header   *types.Header
	receipts types.Receipts
}

// ModifyHeader returns an option to modify the [types.Header] constructed by
// [NewEthBlock]. It SHOULD NOT modify the `Number` and `ParentHash`, but MAY
// modify any other field.
func ModifyHeader(fn func(*types.Header)) EthBlockOption {
	return options.Func[ethBlockProperties](func(p *ethBlockProperties) {
		fn(p.header)
	})
}

// WithReceipts returns an option to set the receipts of a block constructed by
// [NewEthBlock].
func WithReceipts(rs types.Receipts) EthBlockOption {
	return options.Func[ethBlockProperties](func(p *ethBlockProperties) {
		p.receipts = slices.Clone(rs)
	})
}

// A BlockOption configures the default block properties created by [NewBlock].
type BlockOption = options.Option[blockProperties]

// NewBlock constructs an SAE block, wrapping the raw Ethereum block.
func NewBlock(tb testing.TB, eth *types.Block, parent, lastSettled *blocks.Block, opts ...BlockOption) *blocks.Block {
	tb.Helper()

	props := options.ApplyTo(&blockProperties{}, opts...)
	if props.logger == nil {
		props.logger = saetest.NewTBLogger(tb, logging.Warn)
	}

	b, err := blocks.New(eth, parent, lastSettled, props.logger)
	require.NoError(tb, err, "blocks.New()")
	return b
}

type blockProperties struct {
	logger logging.Logger
}

// WithLogger overrides the logger passed to [blocks.New] by [NewBlock].
func WithLogger(l logging.Logger) BlockOption {
	return options.Func[blockProperties](func(p *blockProperties) {
		p.logger = l
	})
}

// NewGenesis constructs a new [core.Genesis], writes it to the database, and
// returns wraps [core.Genesis.ToBlock] with [NewBlock]. It assumes a nil
// [triedb.Config] unless overridden by a [WithTrieDBConfig]. The block is
// marked as both executed and synchronous.
func NewGenesis(tb testing.TB, db ethdb.Database, config *params.ChainConfig, alloc types.GenesisAlloc, opts ...GenesisOption) *blocks.Block {
	tb.Helper()
	conf := &genesisConfig{
		gasTarget: math.MaxUint64,
	}
	options.ApplyTo(conf, opts...)
	gen := conf.genesisSpec
	if gen == nil {
		gen = &core.Genesis{
			Config:    config,
			Timestamp: conf.timestamp,
			Alloc:     alloc,
		}
	}

	tdb := state.NewDatabaseWithConfig(db, conf.tdbConfig).TrieDB()
	_, _, err := core.SetupGenesisBlock(db, tdb, gen)
	require.NoError(tb, err, "core.SetupGenesisBlock()")

	b := NewBlock(tb, gen.ToBlock(), nil, nil)
	require.NoErrorf(tb, b.MarkExecuted(db, gastime.New(gen.Timestamp, conf.gasTarget, conf.gasExcess), time.Time{}, new(big.Int), nil, b.SettledStateRoot()), "%T.MarkExecuted()", b)
	require.NoErrorf(tb, b.MarkSynchronous(), "%T.MarkSynchronous()", b)
	return b
}

type genesisConfig struct {
	tdbConfig   *triedb.Config
	timestamp   uint64
	gasTarget   gas.Gas
	gasExcess   gas.Gas
	genesisSpec *core.Genesis
}

// A GenesisOption configures [NewGenesis].
type GenesisOption = options.Option[genesisConfig]

// WithTrieDBConfig override the [triedb.Config] used by [NewGenesis].
func WithTrieDBConfig(tc *triedb.Config) GenesisOption {
	return options.Func[genesisConfig](func(gc *genesisConfig) {
		gc.tdbConfig = tc
	})
}

// WithGenesisSpec overrides the genesis spec used by [NewGenesis].
func WithGenesisSpec(gen *core.Genesis) GenesisOption {
	return options.Func[genesisConfig](func(gc *genesisConfig) {
		gc.genesisSpec = gen
	})
}

// WithTimestamp overrides the timestamp used by [NewGenesis].
func WithTimestamp(timestamp uint64) GenesisOption {
	return options.Func[genesisConfig](func(gc *genesisConfig) {
		gc.timestamp = timestamp
	})
}

// WithGasTarget overrides the gas target used by [NewGenesis].
func WithGasTarget(target gas.Gas) GenesisOption {
	return options.Func[genesisConfig](func(gc *genesisConfig) {
		gc.gasTarget = target
	})
}

// WithGasExcess overrides the gas excess used by [NewGenesis].
func WithGasExcess(excess gas.Gas) GenesisOption {
	return options.Func[genesisConfig](func(gc *genesisConfig) {
		gc.gasExcess = excess
	})
}

// WithFakeBaseFee creates a new block wrapping the given eth block with a fake
// parent that has its gastime adjusted to produce the desired base fee.
// Upon execution of the resulting block, the fake parent will have its base fee
// set to the desired base fee, thus overriding the base fee mechanism.
// This is useful for tests that need to override the base fee mechanism.
//
// The fake parent is marked as executed with the gastime configured to yield
// the specified base fee. The build time is set to match the block time to
// prevent fast-forwarding the excess during execution.
func WithFakeBaseFee(tb testing.TB, db ethdb.Database, parent *blocks.Block, eth *types.Block, baseFee *big.Int) *blocks.Block {
	tb.Helper()

	target := parent.ExecutedByGasTime().Target()
	desiredExcessGas := desiredExcess(gas.Price(baseFee.Uint64()), target)

	var grandParent *blocks.Block
	if parent.NumberU64() != 0 {
		grandParent = parent.ParentBlock()
	}

	fakeParent := NewBlock(tb, parent.EthBlock(), grandParent, nil)
	// Set the build time to the block time so that we do not fast forward
	// the excess to the block time during execution.
	require.NoError(tb, fakeParent.MarkExecuted(db, gastime.New(eth.Time(), target, desiredExcessGas), time.Time{}, baseFee, nil, parent.PostExecutionStateRoot()))
	require.Equal(tb, baseFee.Uint64(), fakeParent.ExecutedByGasTime().BaseFee().Uint64())

	return NewBlock(tb, eth, fakeParent, nil)
}

// desiredExcess calculates the excess gas needed to produce the desired price.
func desiredExcess(desiredPrice gas.Price, target gas.Gas) gas.Gas {
	// This could be solved directly by calculating D * ln(desiredPrice / P)
	// using floating point math. However, it introduces inaccuracies. So, we
	// use a binary search to find the closest integer solution.
	return gas.Gas(sort.Search(math.MaxInt32, func(excessGuess int) bool { //nolint:gosec // Known to not overflow
		tm := gastime.New(0, target, gas.Gas(excessGuess)) //nolint:gosec // Known to not overflow
		price := tm.Price()
		return price >= desiredPrice
	}))
}
