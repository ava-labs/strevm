// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

//go:build !prod && !nocmpopts

// Package cmputils provides [cmp] options and utilities for their creation.
package cmputils

import (
	"reflect"

	"github.com/google/go-cmp/cmp"
)

// IfIn returns a filtered equivalent of `opt` such that it is only evaluated if
// the [cmp.Path] includes at least one `T`. This is typically used for struct
// fields (and sub-fields).
func IfIn[T any](opt cmp.Option) cmp.Option {
	return cmp.FilterPath(pathIncludes[T], opt)
}

func pathIncludes[T any](p cmp.Path) bool {
	t := reflect.TypeFor[T]()
	for _, step := range p {
		if step.Type() == t {
			return true
		}
	}
	return false
}
