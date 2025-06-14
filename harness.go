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
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/hook"
)

func init() {
	var (
		vm *SinceGenesis
		_  snowcommon.VM                  = vm
		_  adaptor.ChainVM[*blocks.Block] = vm
	)
}

// SinceGenesis is an SAE VM that executes asynchronously immediately,
// stipulating the genesis block as the last synchronous block in [Config].
//
// It is currently only read for use in tests as it creates a new in-memory
// database for every instance.
type SinceGenesis struct {
	*VM // Populated by [SinceGenesis.Initialize]
	// Propagated to [Config]
	Hooks hook.Points
	Now   func() time.Time
}

// Initialize creates an in-memory database, unmarshals the genesis bytes as
// [core.Genesis] JSON to create a genesis block, and constructs a new [VM] that
// provides all other methods.
func (s *SinceGenesis) Initialize(
	ctx context.Context,
	chainCtx *snow.Context,
	_ database.Database,
	genesisBytes []byte,
	_ []byte,
	_ []byte,
	toEngine chan<- snowcommon.Message,
	_ []*snowcommon.Fx,
	_ snowcommon.AppSender,
) error {
	ethdb := rawdb.NewMemoryDatabase()

	genesis := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, genesis); err != nil {
		return err
	}
	sdb := state.NewDatabase(ethdb)
	chainConfig, genesisHash, err := core.SetupGenesisBlock(ethdb, sdb.TrieDB(), genesis)
	if err != nil {
		return err
	}

	batch := ethdb.NewBatch()
	// Being both the "head" and "finalized" block is a requirement of [Config].
	rawdb.WriteHeadBlockHash(batch, genesisHash)
	rawdb.WriteFinalizedBlockHash(batch, genesisHash)
	if err := batch.Write(); err != nil {
		return err
	}

	vm, err := New(
		ctx,
		Config{
			Hooks:                s.Hooks,
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
