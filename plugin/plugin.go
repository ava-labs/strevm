package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/ava-labs/strevm/hook/hooktest"
)

const TargetGasPerSecond = 1_000_000

func main() {
	vm := adaptor.Convert(&sae.SinceGenesis{
		Hooks: hooktest.Simple{
			T: TargetGasPerSecond,
		},
	})

	if err := rpcchainvm.Serve(context.Background(), vm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
