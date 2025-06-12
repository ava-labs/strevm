// Package dummy provides dummy implementations of interfaces required, but not
// actually used by transaction execution. Although these aren't test doubles,
// they effectively fill the same role as a dummy as described in
// https://martinfowler.com/bliki/TestDouble.html.
package dummy

import (
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/consensus"
	"github.com/ava-labs/libevm/core"
	"github.com/ava-labs/libevm/core/types"
)

// ChainContext returns a dummy that returns [Engine] when its Engine() method
// is called, and panics when its GetHeader() method is called.
func ChainContext() core.ChainContext {
	return chainContext{}
}

// Engine returns a dummy that always returns the zero [common.Address] and nil
// error when its Author() method is called.
func Engine() consensus.Engine {
	return engine{}
}

type (
	chainContext struct{}
	engine       struct{ consensus.Engine }
)

func (chainContext) Engine() consensus.Engine                    { return engine{} }
func (chainContext) GetHeader(common.Hash, uint64) *types.Header { panic("unimplemented") }
func (engine) Author(h *types.Header) (common.Address, error)    { return common.Address{}, nil }
