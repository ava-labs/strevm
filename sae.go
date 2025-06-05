package sae

import (
	"errors"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/dustin/go-humanize"
	"golang.org/x/exp/constraints"
)

const (
	stateRootDelaySeconds = 5
	lambda                = 1
	maxGasSecondsPerBlock = 2
)

var (
	errUnimplemented = errors.New("unimplemented")
	errUnsupported   = errors.New("unsupported")
	errShutdown      = errors.New("VM shutting down")
)

// clippedSubtract returns max(0,a-b) without underflow.
func clippedSubtract[T constraints.Unsigned](a, b T) T {
	return boundedSubtract(a, b, 0)
}

// boundedSubtract returns max(floor,a-b) without underflow.
func boundedSubtract[T constraints.Unsigned](a, b, floor T) T {
	if aLim := floor + b; aLim < b /*overflowed*/ || a <= aLim {
		return floor
	}
	return a - b
}

func human[T constraints.Integer](x T) string {
	return humanize.Comma(int64(x))
}

func trieHasher() types.TrieHasher {
	return trie.NewStackTrie(nil)
}

func minGasCharged(tx *types.Transaction) gas.Gas {
	return (gas.Gas(tx.Gas()) + lambda - 1) / lambda
}
