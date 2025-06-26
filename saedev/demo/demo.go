// The demo binary sends simple transactions to an saedev binary's JSONRPC API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/saedev/unsafedev"
	"golang.org/x/term"
)

var pressSpaceToSend bool

func main() {
	flag.BoolVar(&pressSpaceToSend, "press_space_to_send", true, "If true, blocks after signing tx")
	flag.Parse()

	ctx := context.Background()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func must[T any](x T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return x
}

func run(ctx context.Context) error {
	client := must(ethclient.DialContext(ctx, "ws://localhost:9876"))

	key, from := unsafedev.MustPrivateKey(), unsafedev.Address()
	to := common.Address{1, 2, 3, 4, 5, 6}

	printBlockNum := func(ctx context.Context) {
		log.Printf("RPC block number: %v", must(client.BlockNumber(ctx)))
	}
	printBlockNum(ctx)

	printBalances := func(ctx context.Context) {
		log.Println("Sender", must(client.BalanceAt(ctx, from, nil)))
		log.Println("Receiver", must(client.BalanceAt(ctx, to, nil)))
	}
	printBalances(ctx)

	headers := make(chan *types.Header)
	sub := must(client.SubscribeNewHead(ctx, headers))

	signer := types.LatestSignerForChainID(unsafedev.ChainID())
	tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   unsafedev.ChainID(),
		Nonce:     must(client.NonceAt(ctx, from, nil)),
		To:        &to,
		Gas:       params.TxGas,
		Value:     big.NewInt(1e6),
		GasFeeCap: big.NewInt(10e9),
	})
	log.Println("Signed tx", tx.Hash())

	if !shouldSend() {
		return nil
	}

	log.Println("Sending tx", tx.Hash())
	start := time.Now()
	if err := client.SendTransaction(ctx, tx); err != nil {
		return err
	}

	// Although the time printed here isn't indicative of a real-world tx
	// lifecycle, it demonstrates an upper bound on the time between block
	// acceptance and result availability for a single-tx block. It's important
	// to note that in a synchronous setup, this time would still be necessary,
	// only _before_ block acceptance. The key takeaway is that the 5s period
	// before settlment has no bearing on user experience nor tx finality.
	hdr := <-headers
	end := time.Now()
	log.Printf("*#*#*#* Receipt(s) for block %d available in < %s\n", hdr.Number, end.Sub(start))
	printBlockNum(ctx)

	rec := must(client.TransactionReceipt(ctx, tx.Hash()))
	if rec.Status == 1 {
		log.Printf("Tx executed in block %d; used %d gas\n", rec.BlockNumber, rec.GasUsed)
	} else {
		log.Fatalln("Tx execution failed")
	}

	printBalances(ctx)

	sub.Unsubscribe()
	return <-sub.Err()
}

func shouldSend() bool {
	if !pressSpaceToSend {
		return true
	}

	fmt.Println("Press [space] to send tx or [esc] to bail.")

	fd := int(os.Stdin.Fd())
	old := must(term.MakeRaw(fd))
	defer term.Restore(fd, old)

	const (
		space = 0x20
		esc   = 0x1b
	)
	for buf := []byte{0}; buf[0] != space; must(os.Stdin.Read(buf)) {
		if buf[0] == esc {
			return false
		}
	}
	return true
}
