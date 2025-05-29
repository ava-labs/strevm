// The saedev binary runs an in-process SAE EVM and exposes an RPC endpoint for
// development purposes. Notably, it is not an avalanchego node and supports
// only rudimentary PoA orchestration of block building and acceptance.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"testing"

	"github.com/ava-labs/avalanchego/ids"
	snowcommon "github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/snowtest"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	sae "github.com/ava-labs/strevm"
	"go.uber.org/zap/zapcore"
)

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	vm := new(sae.SinceGenesis)

	// test test test test test test test test test test test junk
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		return err
	}
	eoa := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	if crypto.PubkeyToAddress(key.PublicKey) != eoa {
		return fmt.Errorf("address mismatch")
	}

	config := params.TestChainConfig
	config.ChainID = big.NewInt(9876)
	genJSON, err := json.Marshal(&core.Genesis{
		Config:     config,
		Difficulty: big.NewInt(0),
		Alloc: types.GenesisAlloc{
			eoa: {
				Balance: new(big.Int).Mul(big.NewInt(42), big.NewInt(params.Ether)),
			},
		},
	})
	if err != nil {
		return err
	}

	snowCtx := snowtest.Context(tb{}, ids.Empty)
	snowCtx.Log = logging.NewLogger("", logging.NewWrappedCore(
		logging.Verbo, os.Stderr, zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
			MessageKey: "msg",
			TimeKey:    "time",
			LevelKey:   "level",
		}),
	))

	msgs := make(chan snowcommon.Message)
	quit := make(chan struct{})

	if err := vm.Initialize(ctx, snowCtx, nil, genJSON, nil, nil, msgs, nil, nil); err != nil {
		return err
	}
	defer func() {
		close(quit)
		vm.Shutdown(ctx)
	}()
	go func() {
	BuildLoop:
		for {
			select {
			case <-quit:
				return
			case msg := <-msgs:
				if msg != snowcommon.PendingTxs {
					continue BuildLoop
				}
			}

			b, err := vm.BuildBlock(ctx)
			if err != nil {
				log.Fatalf("%T.BuildBlock(): %v", vm, err)
			}
			if err := vm.VerifyBlock(ctx, b); err != nil {
				log.Fatalf("%T.VerifyBlock(): %v", vm, err)
			}
			if err := vm.AcceptBlock(ctx, b); err != nil {
				log.Fatalf("%T.AcceptBlock(): %v", vm, err)
			}
		}
	}()

	handlers, err := vm.CreateHandlers(ctx)
	if err != nil {
		return err
	}
	const addr = "localhost:9876"
	log.Printf("Serving RPC on %q", addr)
	return http.ListenAndServe(addr, handlers[sae.HTTPHandlerKey])
}

type tb struct {
	testing.TB
}

func (tb) Helper() {}

func (tb) TempDir() string {
	dir, err := os.MkdirTemp(os.TempDir(), "sae-dev-*")
	if err != nil {
		log.Fatal(err)
	}
	return dir
}
