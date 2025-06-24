//go:build tools

package sae

// Protects indirect dependencies of tools from being pruned by `go mod tidy`.
import (
	_ "github.com/StephenButtolph/canoto/canoto"
)
