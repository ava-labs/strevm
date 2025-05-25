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
	ctx := context.Background()

	chain := sae.New()

	if err := rpcchainvm.Serve(ctx, adaptor.Convert(chain)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
