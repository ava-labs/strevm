// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package saetest

import (
	"github.com/ava-labs/avalanchego/utils/logging"
	"go.uber.org/zap"
)

// A LogRecorder is a [logging.Logger] that stores all logs as [LogRecord]
// entries for inspection.
type LogRecorder struct {
	logging.NoLog
	records []*LogRecord
}

// A LogRecord is a single entry in a [LogRecorder].
type LogRecord struct {
	Level  logging.Level
	Msg    string
	Fields []zap.Field
}

// Filter returns the recorded logs for which `fn` returns true.
func (l *LogRecorder) Filter(fn func(*LogRecord) bool) []*LogRecord {
	var out []*LogRecord
	for _, r := range l.records {
		if fn(r) {
			out = append(out, r)
		}
	}
	return out
}

// At returns all recorded logs at the specified [logging.Level].
func (l *LogRecorder) At(lvl logging.Level) []*LogRecord {
	return l.Filter(func(r *LogRecord) bool { return r.Level == lvl })
}

// AtLeast returns all recorded logs at or above the specified [logging.Level].
func (l *LogRecorder) AtLeast(lvl logging.Level) []*LogRecord {
	return l.Filter(func(r *LogRecord) bool { return r.Level >= lvl })
}

func (l *LogRecorder) logAt(lvl logging.Level, msg string, fields ...zap.Field) {
	l.records = append(l.records, &LogRecord{
		Level:  lvl,
		Msg:    msg,
		Fields: fields,
	})
}

// Error records a [LogRecord] at [logging.Error].
func (l *LogRecorder) Error(msg string, fields ...zap.Field) {
	l.logAt(logging.Error, msg, fields...)
}

// Fatal records a [LogRecord] at [logging.Fatal].
func (l *LogRecorder) Fatal(msg string, fields ...zap.Field) {
	l.logAt(logging.Fatal, msg, fields...)
}
