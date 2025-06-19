//go:build !prod && !nocmpopts

package gastime

import (
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// CmpOpt returns a configuration for [cmp.Diff] to compare [Time] instances in
// tests.
func CmpOpt() cmp.Option {
	return cmp.Options{
		cmp.AllowUnexported(TimeMarshaler{}),
		cmpopts.IgnoreTypes(canotoData_TimeMarshaler{}),
	}
}
