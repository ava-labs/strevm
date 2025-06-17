package blocks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func ExampleBlock_IfChildSettles() {
	parent := blockBuildingPreference()
	settle, ok := LastToSettleAt(uint64(time.Now().Unix()), parent)
	if !ok {
		return // execution is lagging; please come back soon
	}

	// Returns the (possibly empty) slice of blocks that may be settled by the
	// block being built.
	_ = parent.IfChildSettles(settle)
}

func blockBuildingPreference() *Block {
	b, _ := New(&types.Block{}, nil, nil, nil)
	return b
}

type logRecorder struct {
	logging.NoLog
	records []logRecord
}

type logRecord struct {
	level  logging.Level
	msg    string
	fields []zap.Field
}

func (l *logRecorder) Error(msg string, fields ...zap.Field) {
	l.records = append(l.records, logRecord{
		level:  logging.Error,
		msg:    msg,
		fields: fields,
	})
}

func (l *logRecorder) Fatal(msg string, fields ...zap.Field) {
	l.records = append(l.records, logRecord{
		level:  logging.Fatal,
		msg:    msg,
		fields: fields,
	})
}

func TestSettlementInvariants(t *testing.T) {
	t.Parallel()

	parent := newBlock(t, newEthBlock(5, 5, nil), nil, nil)
	lastSettled := newBlock(t, newEthBlock(3, 3, nil), nil, nil)

	b := newBlock(t, newEthBlock(0, 0, parent.Block), parent, lastSettled)

	t.Run("before_MarkSettled", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		assert.ErrorIs(t, b.WaitUntilSettled(ctx), context.DeadlineExceeded, "WaitUntilSettled()")

		assert.True(t, b.ParentBlock().Equal(parent), "ParentBlock().Equal([constructor arg])")
		assert.True(t, b.LastSettled().Equal(lastSettled), "LastSettled().Equal([constructor arg])")
	})
	if t.Failed() {
		t.FailNow()
	}

	b.MarkSettled()

	t.Run("after_MarkSettled", func(t *testing.T) {
		assert.NoError(t, b.WaitUntilSettled(context.Background()), "WaitUntilSettled()")

		var rec logRecorder
		b.log = &rec

		assert.Nil(t, b.ParentBlock(), "ParentBlock()")
		assert.Len(t, rec.records, 1, "Number of ERROR or FATAL logs")
		assert.Nil(t, b.LastSettled(), "LastSettled()")
		assert.Len(t, rec.records, 2, "Number of ERROR or FATAL logs")
		b.MarkSettled()
		assert.Len(t, rec.records, 3, "Number of ERROR or FATAL logs")
		if t.Failed() {
			t.FailNow()
		}

		want := []logRecord{
			{
				level: logging.Error,
				msg:   getParentOfSettledMsg,
			},
			{
				level: logging.Error,
				msg:   getSettledOfSettledMsg,
			},
			{
				level: logging.Fatal,
				msg:   blockResettledMsg,
			},
		}
		if diff := cmp.Diff(want, rec.records, cmp.AllowUnexported(logRecord{})); diff != "" {
			t.Errorf("ERROR + FATAL logs diff (-want +got):\n%s", diff)
		}
	})
}

func TestSettles(t *testing.T) {
	lastSettledAtHeight := map[uint64]uint64{
		0: 0, // genesis block is self-settling by definition
		1: 0,
		2: 0,
		3: 0,
		4: 1,
		5: 1,
		6: 3,
		7: 3,
		8: 3,
		9: 7,
	}
	wantSettles := map[uint64][]uint64{
		// It is not valid to call Settles() on the genesis block
		1: nil,
		2: nil,
		3: nil,
		4: {1},
		5: nil,
		6: {2, 3},
		7: nil,
		8: nil,
		9: {4, 5, 6, 7},
	}
	blocks := newChain(t, 0, 10, lastSettledAtHeight)

	numsToBlocks := func(nums ...uint64) []*Block {
		bs := make([]*Block, len(nums))
		for i, n := range nums {
			bs[i] = blocks[n]
		}
		return bs
	}

	type testCase struct {
		name      string
		got, want []*Block
	}
	var tests []testCase

	for num, wantNums := range wantSettles {
		tests = append(tests, testCase{
			name: fmt.Sprintf("Block(%d).Settles()", num),
			got:  blocks[num].Settles(),
			want: numsToBlocks(wantNums...),
		})
	}

	for _, b := range blocks[1:] {
		tests = append(tests, testCase{
			name: fmt.Sprintf("Block(%d).IfChildSettles([same as parent])", b.Height()),
			got:  b.IfChildSettles(b.LastSettled()),
			want: nil,
		})
	}

	tests = append(tests, []testCase{
		{
			got:  blocks[7].IfChildSettles(blocks[3]),
			want: nil,
		},
		{
			got:  blocks[7].IfChildSettles(blocks[4]),
			want: numsToBlocks(4),
		},
		{
			got:  blocks[7].IfChildSettles(blocks[5]),
			want: numsToBlocks(4, 5),
		},
		{
			got:  blocks[7].IfChildSettles(blocks[6]),
			want: numsToBlocks(4, 5, 6),
		},
	}...)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, tt.got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("diff (-want +got):\n%s", diff)
			}
		})
	}
}
