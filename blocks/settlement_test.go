package blocks

import (
	"context"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/core/types"
	"github.com/google/go-cmp/cmp"
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
		assert.Len(t, rec.records, 1)
		assert.Nil(t, b.LastSettled(), "LastSettled()")
		assert.Len(t, rec.records, 2)
		b.MarkSettled()
		assert.Len(t, rec.records, 3)

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
