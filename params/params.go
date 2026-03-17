// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package params declares [Streaming Asynchronous Execution] (SAE) parameters.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package params

import "time"

// Lambda is the denominator for computing the minimum gas consumed per
// transaction. For a transaction with gas limit `g`, the minimum consumption is
// ceil(g/Lambda).
const Lambda = 2

// Tau is the minimum duration between a block's execution completing and the
// resulting state changes being settled in a later block. Note that this period
// has no effect on the availability nor finality of results, both of which are
// immediate at the time of executing an individual transaction.
const (
	Tau        = TauSeconds * time.Second
	TauSeconds = 5
)

// MaxQueueWallTime is the maximum wall-clock duration a block can remain in
// the execution queue before processing begins. It is derived from
// ω_Q / R = 2τλ (see ACP-194), which assumes the executor drains the queue at
// the gas capacity rate R. This assumption may not be true in practice.
const MaxQueueWallTime = 2 * Tau * Lambda
