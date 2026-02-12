// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package params

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrieDBCommitHeights(t *testing.T) {
	const e = CommitTrieDBEvery

	for num, want := range map[uint64]bool{
		e - 1:   false,
		e:       true,
		e + 1:   false,
		2*e - 1: false,
		2 * e:   true,
		2*e + 1: false,
	} {
		if got := CommitTrieDB(num); got != want {
			t.Errorf("CommitTrieDB(%d) got %t want %t", num, got, want)
		}
	}

	for num, want := range map[uint64]uint64{
		0:       0,
		e - 1:   0,
		e:       e,
		e + 1:   e,
		2*e - 1: e,
		2 * e:   2 * e,
		2*e + 1: 2 * e,
		3*e - 1: 2 * e,
	} {
		if got := LastCommittedTrieDBHeight(num); got != want {
			t.Errorf("LastCommitedTrieDBHeight(%d) got %d; want %d", num, got, want)
		}
	}

	var last uint64
	for num := range uint64(20 * e) {
		if CommitTrieDB(num) {
			last = num
		}
		if got, want := LastCommittedTrieDBHeight(num), last; got != want {
			t.Errorf("LastCommitedTrieDBHeight(%d) got %d; want %d", num, got, want)
		}
	}
}

func TestRawDBKeyForBlock(t *testing.T) {
	tests := []struct {
		namespace string
		num       uint64
		want      string
	}{
		{
			namespace: "foo",
			num:       0,
			want:      rawDBPrefix + "foo-" + string(make([]byte, 8)),
		},
		{
			namespace: "bar",
			num:       42,
			want:      rawDBPrefix + "bar-" + string([]byte{7: 42}),
		},
		{
			namespace: "bazza",
			num:       42 << (7 * 8),
			want:      rawDBPrefix + "bazza-" + string([]byte{0: 42, 7: 0}),
		},
	}

	for _, tt := range tests {
		got := RawDBKeyForBlock(tt.namespace, tt.num)
		assert.Equalf(t, tt.want, string(got), "RawDBKeyForBlock(%q, %d)", tt.namespace, tt.num)
	}
}
