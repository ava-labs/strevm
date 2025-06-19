package blocks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethdb"
	"github.com/ava-labs/strevm/gastime"
	"github.com/ava-labs/strevm/proxytime"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

		assert.True(t, b.ParentBlock().equalForTests(parent), "ParentBlock().Equal([constructor arg])")
		assert.True(t, b.LastSettled().equalForTests(lastSettled), "LastSettled().Equal([constructor arg])")
	})
	if t.Failed() {
		t.FailNow()
	}

	require.NoError(t, b.MarkSettled(), "first call to MarkSettled()")

	t.Run("after_MarkSettled", func(t *testing.T) {
		assert.NoError(t, b.WaitUntilSettled(context.Background()), "WaitUntilSettled()")

		var rec logRecorder
		b.log = &rec

		assert.Nil(t, b.ParentBlock(), "ParentBlock()")
		assert.Len(t, rec.records, 1, "Number of ERROR or FATAL logs")
		assert.Nil(t, b.LastSettled(), "LastSettled()")
		assert.Len(t, rec.records, 2, "Number of ERROR or FATAL logs")
		assert.ErrorIs(t, b.MarkSettled(), errBlockResettled, "second call to MarkSettled()")
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
				level: logging.Error,
				msg:   errBlockResettled.Error(),
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

func (b *Block) markExecutedForTests(tb testing.TB, db ethdb.Database, tm *gastime.Time) {
	tb.Helper()
	require.NoError(tb, b.MarkExecuted(db, tm, time.Time{}, nil, common.Hash{}), "MarkExecuted()")
}

func TestLastToSettleAt(t *testing.T) {
	blocks := newChain(t, 0, 20, nil)
	t.Run("helper_invariants", func(t *testing.T) {
		for i, b := range blocks {
			require.Equal(t, uint64(i), b.Height())
			require.Equal(t, b.Time(), b.Height())
		}
	})

	db := rawdb.NewMemoryDatabase()
	tm := gastime.New(0, 5 /*target*/, 0)
	require.Equal(t, gas.Gas(10), tm.Rate())

	blocks[0].markExecutedForTests(t, db, tm) // 0.0
	tm.Tick(13)
	blocks[1].markExecutedForTests(t, db, tm) // 1.3
	tm.Tick(20)
	blocks[2].markExecutedForTests(t, db, tm) // 3.3
	tm.Tick(5)
	blocks[3].markExecutedForTests(t, db, tm) // 3.8
	tm.Tick(23)
	blocks[4].markExecutedForTests(t, db, tm) // 6.1
	tm.Tick(9)
	blocks[5].markExecutedForTests(t, db, tm) // 7.0
	tm.Tick(10)
	blocks[6].markExecutedForTests(t, db, tm) // 8.0
	tm.Tick(1)
	blocks[7].markExecutedForTests(t, db, tm) // 8.1
	tm.Tick(50)
	blocks[8].markExecutedForTests(t, db, tm) // 13.1

	tests := []struct {
		name     string
		settleAt uint64
		parent   *Block
		wantOK   bool
		want     *Block
	}{
		{
			settleAt: 3,
			parent:   blocks[5],
			wantOK:   true,
			want:     blocks[1],
		},
		{
			settleAt: 4,
			parent:   blocks[9],
			wantOK:   true,
			want:     blocks[3],
		},
		{
			settleAt: 4,
			parent:   blocks[8],
			wantOK:   true,
			want:     blocks[3],
		},
		{
			settleAt: 7,
			parent:   blocks[10],
			wantOK:   true,
			want:     blocks[5],
		},
		{
			settleAt: 9,
			parent:   blocks[8],
			wantOK:   true,
			want:     blocks[7],
		},
		{
			settleAt: 9,
			parent:   blocks[9],
			// The current implementation is very coarse-grained and MAY return
			// false negatives that would simply require a retry after some
			// indeterminate period of time. Even though the execution time of
			// `blocks[8]` guarantees that `blocks[9]` MUST finish execution
			// after the settlement time, our current implementation doesn't
			// check this. It is expected that this specific test case will one
			// day fail, at which point it MUST be updated to want `blocks[7]`.
			wantOK: false,
		},
		{
			settleAt: 15,
			parent:   blocks[18],
			wantOK:   false,
		},
	}

	t.Run("validate_setup", func(t *testing.T) {
		frac := func(g gas.Gas) (f proxytime.FractionalSecond[gas.Gas]) {
			f.Numerator = g
			f.Denominator = tm.Rate()
			return f
		}

		tests := []struct {
			Unix     uint64
			Fraction proxytime.FractionalSecond[gas.Gas]
		}{
			{0, frac(0)},
			{1, frac(3)},
			{3, frac(3)},
			{3, frac(8)},
			{6, frac(1)},
			{7, frac(0)},
			{8, frac(0)},
			{8, frac(1)},
			{13, frac(1)},
		}
		for i, tt := range tests {
			got := blocks[i].ExecutedByGasTime()
			assert.Equal(t, tt.Unix, got.Unix())
			assert.Equal(t, tt.Fraction, got.Fraction())
		}
	})
	if t.Failed() {
		t.Fatalf("Invalid test setup; failing early as results will be corrupted")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOK := LastToSettleAt(tt.settleAt, tt.parent)
			require.Equalf(t, tt.wantOK, gotOK, "LastToSettleAt(%d, [parent height %d])", tt.settleAt, tt.parent.Height())
			if tt.wantOK {
				require.Equal(t, tt.want.Height(), got.Height(), "LastToSettleAt(%d, [parent height %d])", tt.settleAt, tt.parent.Height())
			}
		})
	}
}
