// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/snow"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/libevm/triedb"

	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/blocks"
)

var _ adaptor.ChainVM[*blocks.Block] = (*SinceGenesis)(nil)

// SinceGenesis is a harness around a [VM], providing an `Initialize` method
// that treats the chain as being asynchronous since genesis.
type SinceGenesis struct {
	*VM // created by [SinceGenesis.Initialize]

	config Config
}

// NewSinceGenesis constructs a new [SinceGenesis].
func NewSinceGenesis(c Config) *SinceGenesis {
	return &SinceGenesis{config: c}
}

// Initialize initializes the VM.
func (vm *SinceGenesis) Initialize(
	ctx context.Context,
	snowCtx *snow.Context,
	avaDB database.Database,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	fxs []*snowcommon.Fx,
	appSender snowcommon.AppSender,
) error {
	db := newEthDB(avaDB)
	tdb := triedb.NewDatabase(db, vm.config.TrieDBConfig)

	genesis := new(core.Genesis)
	if err := json.Unmarshal(genesisBytes, genesis); err != nil {
		return fmt.Errorf("json.Unmarshal(%T): %v", genesis, err)
	}
	config, _, err := core.SetupGenesisBlock(db, tdb, genesis)
	if err != nil {
		return fmt.Errorf("core.SetupGenesisBlock(...): %v", err)
	}

	genBlock, err := blocks.New(genesis.ToBlock(), nil, nil, snowCtx.Log)
	if err != nil {
		return fmt.Errorf("blocks.New(%T.ToBlock(), ...): %v", genesis, err)
	}
	if err := genBlock.MarkSynchronous(vm.config.Hooks, db, 0 /*gas excess*/); err != nil {
		return fmt.Errorf("%T{genesis}.MarkSynchronous(): %v", genBlock, err)
	}

	inner, err := NewVM(ctx, vm.config, snowCtx, config, db, genBlock, appSender)
	if err != nil {
		return err
	}
	vm.VM = inner
	return nil
}

// canonicaliseLastSynchronous writes all necessary information to the database
// to have the block be considered canonical by SAE. If there are any canonical
// blocks at a height greater than the provided block then this function is a
// no-op, which makes it effectively idempotent with respect to the rest of SAE
// processing.
func canonicaliseLastSynchronous(db ethdb.Database, block *blocks.Block) error {
	if !block.Synchronous() {
		return fmt.Errorf("only synchronous block can be canonicalised: %d / %#x is async", block.NumberU64(), block.Hash())
	}
	num := block.NumberU64()
	if accepted, _ := rawdb.ReadAllCanonicalHashes(db, num+1, num+2, 1); len(accepted) > 0 {
		// If any other block has been accepted then the genesis block must have
		// been canonicalised in a previous initialisation.
		return nil
	}

	h := block.Hash()
	b := db.NewBatch()
	rawdb.WriteCanonicalHash(b, h, num)
	block.SetAsHeadBlock(b)
	rawdb.WriteFinalizedBlockHash(b, h)
	return b.Write()
}

// Shutdown gracefully closes the VM.
func (vm *SinceGenesis) Shutdown(ctx context.Context) error {
	if vm.VM == nil {
		return nil
	}
	return vm.VM.Shutdown(ctx)
}
