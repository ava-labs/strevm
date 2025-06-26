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
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/params"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/hook/hooktest"
	"github.com/ava-labs/strevm/saedev/unsafedev"
	"go.uber.org/zap/zapcore"
)

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	vm := &sae.SinceGenesis{
		Hooks: hooktest.Simple{
			T: 10e6,
		},
	}

	// test test test test test test test test test test test junk
	key := unsafedev.MustPrivateKey()
	eoa := unsafedev.Address()
	if crypto.PubkeyToAddress(key.PublicKey) != eoa {
		return fmt.Errorf("address mismatch")
	}

	config := params.MergedTestChainConfig
	config.ChainID = unsafedev.ChainID()
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
			if err := vm.SetPreference(ctx, b.ID()); err != nil {
				log.Fatalf("%T.SetPreference(): %v", vm, err)
			}
		}
	}()

	handlers, err := vm.CreateHandlers(ctx)
	if err != nil {
		return err
	}
	const addr = "localhost:9876"
	log.Printf("Serving RPC on %q", addr)
	return http.ListenAndServe(addr, handlers[sae.WSHandlerKey])
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
