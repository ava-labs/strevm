// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package sae

import (
	"testing"

	"github.com/ava-labs/avalanchego/version"
	"github.com/stretchr/testify/require"
)

func TestSinceGenesisBeforeInit(t *testing.T) {
	ctx := t.Context()
	sut := NewSinceGenesis(Config{})
	t.Run("Version", func(t *testing.T) {
		got, err := sut.Version(ctx)
		require.NoError(t, err)
		require.Equal(t, version.Current.String(), got)
	})
	require.NoErrorf(t, sut.Shutdown(t.Context()), "%T.Shutdown()")
}
