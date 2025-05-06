package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/upgrade"
	"github.com/ava-labs/avalanchego/vms/proposervm"
	"github.com/ava-labs/avalanchego/vms/rpcchainvm"
	sae "github.com/ava-labs/strevm"
	"github.com/ava-labs/strevm/adaptor"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	ctx := context.Background()

	var chain block.ChainVM = adaptor.Convert(sae.New())
	if false {
		chain = proposervm.New(chain, proposervm.Config{
			Upgrades:    upgrade.Config{},
			MinBlkDelay: time.Second,
			Registerer:  prometheus.DefaultRegisterer,
		})
	}

	if err := rpcchainvm.Serve(ctx, chain); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
