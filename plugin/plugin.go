package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
)

func main() {
	vm := adaptor.Convert(new(sae.SinceGenesis))

	if err := rpcchainvm.Serve(context.Background(), vm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
