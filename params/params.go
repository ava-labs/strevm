// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package params declares [Streaming Asynchronous Execution] (SAE) parameters.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package params

const (
	// Lambda is the denominator for computing the minimum gas consumed per
	// transaction. For a transaction with gas limit `g`, the minimum
	// consumption is ceil(g/Lambda).
	Lambda = 2

	// Tau is the number of seconds after a block has finished executing before
	// it is settled.
	Tau = 5
)
