package sae

import (
	"errors"

	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/dustin/go-humanize"
	"golang.org/x/exp/constraints"
)

const (
	maxGasPerChunk        = gas.Gas(50e6) // C-Chain goal of 50Mg/s by September
	stateRootDelaySeconds = 5
)

var errUnimplemented = errors.New("unimplemented")

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
