// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package ethtests

import (
	"context"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/libevm/options"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/libevm/triedb"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks/blockstest"
	"github.com/ava-labs/strevm/saetest"
	"github.com/ava-labs/strevm/saexec"
)

// SUT is the system under test, primarily the [Executor].
type SUT struct {
	*saexec.Executor
	Chain  *blockstest.ChainBuilder
	Wallet *saetest.Wallet
	Logger *saetest.TBLogger
	DB     ethdb.Database
}

type sutOptions struct {
	triedbConfig   *triedb.Config
	genesisSpec    *core.Genesis
	chainConfig    *params.ChainConfig
	snapshotConfig *snapshot.Config
}

type sutOption = options.Option[sutOptions]

func withTrieDBConfig(tdbConfig *triedb.Config) sutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.triedbConfig = tdbConfig
	})
}

func withGenesisSpec(genesisSpec *core.Genesis) sutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.genesisSpec = genesisSpec
	})
}

func withChainConfig(chainConfig *params.ChainConfig) sutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.chainConfig = chainConfig
	})
}

func withSnapshotConfig(snapshotConfig *snapshot.Config) sutOption {
	return options.Func[sutOptions](func(o *sutOptions) {
		o.snapshotConfig = snapshotConfig
	})
}

// newSUT returns a new SUT. Any >= [logging.Error] on the logger will also
// cancel the returned context, which is useful when waiting for blocks that
// can never finish execution because of an error.
func newSUT(tb testing.TB, engine consensus.Engine, opts ...sutOption) (context.Context, SUT) {
	tb.Helper()

	// This is specifically set to [logging.Error] to ensure that the warn log in execution queue
	// does not cause the test to fail.
	logger := saetest.NewTBLogger(tb, logging.Error)
	ctx := logger.CancelOnError(tb.Context())
	db := rawdb.NewMemoryDatabase()

	conf := options.ApplyTo(&sutOptions{}, opts...)
	chainConfig := conf.chainConfig
	if chainConfig == nil {
		chainConfig = saetest.ChainConfig()
	}
	tdbConfig := conf.triedbConfig
	if tdbConfig == nil {
		tdbConfig = &triedb.Config{}
	}
	wallet := saetest.NewUNSAFEWallet(tb, 1, types.LatestSigner(chainConfig))
	alloc := saetest.MaxAllocFor(wallet.Addresses()...)
	genesisSpec := conf.genesisSpec
	if genesisSpec == nil {
		genesisSpec = &core.Genesis{
			Config: chainConfig,
			Alloc:  alloc,
		}
	}
	snapshotConfig := conf.snapshotConfig
	if snapshotConfig == nil {
		snapshotConfig = &snapshot.Config{
			CacheSize:  128, // MB
			AsyncBuild: true,
		}
	}

	target := 1e6
	genesis := blockstest.NewGenesisFromSpec(tb, db, genesisSpec, blockstest.WithTrieDBConfig(tdbConfig), blockstest.WithGasTarget(target))

	blockOpts := blockstest.WithBlockOptions(
		blockstest.WithLogger(logger),
	)
	chain := blockstest.NewChainBuilder(genesis, blockOpts)

	reader := newReaderAdapter(chain, db, chainConfig, logger)
	hooks := newTestConsensusHooks(engine, reader, target)
	e, err := saexec.New(genesis, chain.GetBlock, chainConfig, db, tdbConfig, *snapshotConfig, hooks, logger)
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
