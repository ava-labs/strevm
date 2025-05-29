package sae

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/strevm/adaptor"
)

func init() {
	var (
		vm *SinceGenesis
		_  snowcommon.VM           = vm
		_  core.ChainContext       = vm
		_  adaptor.ChainVM[*Block] = vm
	)
}

// SinceGenesis is an SAE VM that executes asynchronously immediately,
// stipulating the genesis block as the last synchronous block in [Config].
type SinceGenesis struct {
	*VM                  // Populated by [SinceGenesis.Initialize]
	Now func() time.Time // Propagated to [Config]
}

func (s *SinceGenesis) Initialize(
	ctx context.Context,
	chainCtx *snow.Context,
	db database.Database,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- snowcommon.Message,
	fxs []*snowcommon.Fx,
	appSender snowcommon.AppSender,
) error {
	ethdb := rawdb.NewMemoryDatabase() // TODO(arr4n) wrap the [database.Database] argument

	genesis := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, genesis); err != nil {
		return err
	}
	sdb := state.NewDatabase(ethdb)
	chainConfig, genesisHash, err := core.SetupGenesisBlock(ethdb, sdb.TrieDB(), genesis)
	if err != nil {
		return err
	}

	vm, err := New(
		ctx,
		Config{
			ChainConfig:          chainConfig,
			DB:                   ethdb,
			LastSynchronousBlock: genesisHash,
			ToEngine:             toEngine,
			SnowCtx:              chainCtx,
			Now:                  s.Now,
		},
	)
	if err != nil {
		return err
	}
	s.VM = vm
	return nil
}
