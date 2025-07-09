// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build !prod && !nocmpopts

package proxytime

import (
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/ava-labs/strevm/cmputils"
)

// A CmpRateInvariantsBy value configures [CmpOpt] treatment of rate-invariant
// values.
type CmpRateInvariantsBy uint64

// Valid [CmpRateInvariantsBy] values.
const (
	CmpRateInvariantsByPointer CmpRateInvariantsBy = iota
	CmpRateInvariantsByValue
	IgnoreRateInvariants
)

// CmpOpt returns a configuration for [cmp.Diff] to compare [Time] instances in
// tests. The option will only be applied to the specific [Duration] type.
func CmpOpt[D Duration](invariants CmpRateInvariantsBy) cmp.Option {
	return cmp.Options{
		cmp.AllowUnexported(Time[D]{}),
		cmpopts.IgnoreTypes(canotoData_Time{}),
		invariantsOpt[D](invariants),
	}
}

func invariantsOpt[D Duration](by CmpRateInvariantsBy) (opt cmp.Option) {
	defer func() {
		opt = cmputils.IfIn[*Time[D]](opt)
	}()

	switch by {
	case IgnoreRateInvariants:
		return cmpopts.IgnoreTypes([]*D{})

	case CmpRateInvariantsByValue:
		return cmp.Transformer("rate_invariants_as_values", func(ptrs []*D) (vals []D) {
			for _, x := range ptrs {
				vals = append(vals, *x) // [Time.SetRateInvariants] requires that they aren't nil.
			}
			return vals
		})

	case CmpRateInvariantsByPointer:
		return cmp.Comparer(func(a, b []*D) bool {
			if len(a) != len(b) {
				return false
			}
			for i := range a {
				if a[i] != b[i] {
					return false
				}
			}
			return true
		})
	}

	panic(fmt.Sprintf("Unsupported %T value: %d", by, by))
}
