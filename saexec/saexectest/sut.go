// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saexectest

import (
	"context"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/hook"
	saehookstest "github.com/ava-labs/strevm/hook/hookstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
	"github.com/stretchr/testify/require"
)

// SUT is the system under test, primarily the [Executor].
type SUT struct {
	*saexec.Executor
	Chain  *blockstest.ChainBuilder
	Wallet *saetest.Wallet
	Logger logging.Logger
	DB     ethdb.Database
}

type sutOptions struct {
	triedbConfig *triedb.Config
	genesisSpec  *core.Genesis
	chainConfig  *params.ChainConfig
}

type SutOption = options.Option[sutOptions]

func WithTrieDBConfig(tdbConfig *triedb.Config) SutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.triedbConfig = tdbConfig
	})
}

func WithGenesisSpec(genesisSpec *core.Genesis) SutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.genesisSpec = genesisSpec
	})
}

func WithChainConfig(chainConfig *params.ChainConfig) SutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.chainConfig = chainConfig
	})
}

// NewSUT returns a new SUT. Any >= [logging.Error] on the logger will also
// cancel the returned context, which is useful when waiting for blocks that
// can never finish execution because of an error.
func NewSUT(tb testing.TB, hooks hook.Points, opts ...SutOption) (context.Context, SUT) {
	tb.Helper()

	logger := saetest.NewTBLogger(tb, logging.Warn)
	ctx := logger.CancelOnError(tb.Context())
	db := rawdb.NewMemoryDatabase()

	conf := options.ApplyTo(&sutOptions{}, opts...)
	chainConfig := conf.chainConfig
	if chainConfig == nil {
		chainConfig = params.AllDevChainProtocolChanges
	}
	tdbConfig := conf.triedbConfig
	if tdbConfig == nil {
		tdbConfig = &triedb.Config{}
	}
	genesisSpec := conf.genesisSpec
	if genesisSpec == nil {
		genesisSpec = &core.Genesis{
			Config: chainConfig,
		}
	}

	wallet := saetest.NewUNSAFEWallet(tb, 1, types.LatestSigner(chainConfig))
	alloc := saetest.MaxAllocFor(wallet.Addresses()...)
	genesis := blockstest.NewGenesis(tb, db, chainConfig, alloc, blockstest.WithTrieDBConfig(tdbConfig))

	chain := blockstest.NewChainBuilder(genesis)
	chain.SetDefaultOptions(blockstest.WithBlockOptions(
		blockstest.WithLogger(logger),
	))
	src := saexec.BlockSource(func(h common.Hash, n uint64) *blocks.Block {
		b, ok := chain.GetBlock(h, n)
		if !ok {
			return nil
		}
		return b
	})

	e, err := saexec.New(genesis, src, chainConfig, db, tdbConfig, hooks, logger)
	require.NoError(tb, err, "New()")
	tb.Cleanup(func() {
		require.NoErrorf(tb, e.Close(), "%T.Close()", e)
	})

	return ctx, SUT{
		Executor: e,
		Chain:    chain,
		Wallet:   wallet,
		Logger:   logger,
		DB:       db,
	}
}

func DefaultHooks() *saehookstest.Stub {
	return &saehookstest.Stub{Target: 1e6}
}

func (sut *SUT) InsertChain(ctx context.Context, tb testing.TB, blocks types.Blocks) (int, error) {
	tb.Helper()
	for i, b := range blocks {
		wb, err := sut.Chain.InsertBlock(tb, b)
		if err != nil {
			return i, err
		}
		if err := sut.Enqueue(ctx, wb); err != nil {
			return i, err
		}
	}

	last := sut.Chain.Last()
	require.NoErrorf(tb, last.WaitUntilExecuted(ctx), "%T.Last().WaitUntilExecuted()")
	return len(blocks), nil
}
