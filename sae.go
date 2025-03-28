package sae

import (
	"errors"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/dustin/go-humanize"
	"golang.org/x/exp/constraints"
)

const (
	maxGasPerChunk        = gas.Gas(10e6)
	stateRootDelaySeconds = 5
)

var errUnimplemented = errors.New("unimplemented")

// clippedSubtract returns max(0,a-b) without underflow.
func clippedSubtract[T constraints.Unsigned](a, b T) T {
	if b >= a {
		return 0
	}
	return a - b
}

func human[T constraints.Integer](x T) string {
	return humanize.Comma(int64(x))
}
