//go:build !prod && !nocmpopts

package saexec

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/arr4n/sink"
	"github.com/ava-labs/libevm/core/state/snapshot"
	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/queue"
	"github.com/ava-labs/strevm/saetest"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// CmpOpt returns a configuration for [cmp.Diff] to compare [Executor] instances
// in tests.
func CmpOpt(ctx context.Context) cmp.Option {
	var zero Executor

	return cmp.Options{
		blocks.CmpOpt(),
		cmp.AllowUnexported(
			Executor{},
			executionScratchSpace{},
		),
		saetest.CmpChainConfig(),
		saetest.CmpTimes(),
		saetest.CmpStateDBs(),
		cmpopts.IgnoreTypes(
			zero.quit, zero.spawned,
			zero.headEvents, zero.chainEvents, zero.logEvents,
			snapshot.Tree{},
		),
		saetest.CmpIgnoreLoggers(),
		saetest.CmpIgnoreLowLevelDBs(),
		cmp.Transformer("atomic_block_pointer", func(p *atomic.Pointer[blocks.Block]) *blocks.Block {
			return p.Load()
		}),
		cmp.Transformer("queue_to_slice", func(mon sink.Monitor[*queue.FIFO[*blocks.Block]]) []*blocks.Block {
			bs, err := sink.FromMonitor(
				ctx, mon,
				func(*queue.FIFO[*blocks.Block]) bool { return true },
				func(q *queue.FIFO[*blocks.Block]) ([]*blocks.Block, error) {
					var bs []*blocks.Block
					for q.Len() > 0 {
						bs = append(bs, q.Pop())
					}
					for _, b := range bs {
						q.Push(b)
					}
					return bs, nil
				},
			)
			if err != nil {
				panic(fmt.Sprintf("Transforming %T to %T: %v", mon, bs, err))
			}
			return bs
		}),
	}
}
