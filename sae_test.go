package sae

import (
	"context"
	"testing"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/rlp"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

func TestBasicRoundTrip(t *testing.T) {
	ctx := context.Background()

	chain := New()

	ethBlock := types.NewBlockWithHeader(&types.Header{})
	blockBuf, err := rlp.EncodeToBytes(ethBlock)
	require.NoErrorf(t, err, "rlp.EncodeToBytes(%T)", ethBlock)

	block, err := chain.ParseBlock(ctx, blockBuf)
	require.NoErrorf(t, err, "%T.ParseBlock()", chain)
	require.NoErrorf(t, block.Verify(ctx), "%T.Verify()", block)

	require.NoErrorf(t, block.Accept(ctx), "%T.Accept()", block)

	require.NoError(t, chain.Shutdown(ctx))
}
