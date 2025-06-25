// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build !prod && !nocmpopts

package proxytime

import (
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
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
	opts := cmp.Options{
		cmp.AllowUnexported(Time[D]{}),
		cmpopts.IgnoreTypes(canotoData_Time{}),
	}

	var cmpInvariants cmp.Option
	switch invariants {
	case IgnoreRateInvariants:
		cmpInvariants = cmpopts.IgnoreFields(Time[D]{}, "rateInvariants")

	case CmpRateInvariantsByValue:
		cmpInvariants = cmp.Transformer("rate_invariants_as_values", func(x *D) D {
			return *x
		})

	case CmpRateInvariantsByPointer:
		cmpInvariants = cmp.Comparer(func(a, b []*D) bool {
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

	default:
		panic(fmt.Sprintf("Unsupported %T value: %d", invariants, invariants))
	}

	return append(opts, cmpOnlyIfTimeField[D](cmpInvariants))
}

func cmpOnlyIfTimeField[D Duration](opt cmp.Option) cmp.Option {
	return cmp.FilterPath(func(p cmp.Path) bool {
		t := reflect.TypeFor[*Time[D]]()
		for _, step := range p {
			if step.Type() == t {
				return true
			}
		}
		return false
	}, opt)
}
