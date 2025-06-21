package sae

import (
	"errors"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/trie"
	"github.com/ava-labs/strevm/intmath"
	"github.com/dustin/go-humanize"
	"golang.org/x/exp/constraints"
)

var (
	errUnimplemented = errors.New("unimplemented")
	errUnsupported   = errors.New("unsupported")
	errShutdown      = errors.New("VM shutting down")
)

// clippedSubtract returns max(0,a-b) without underflow.
func clippedSubtract[T constraints.Unsigned](a, b T) T {
	return intmath.BoundedSubtract(a, b, 0)
}

func human[T constraints.Integer](x T) string {
	return humanize.Comma(int64(x))
}

func trieHasher() types.TrieHasher {
	return trie.NewStackTrie(nil)
}
