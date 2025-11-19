// Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

// Package params declares [Streaming Asynchronous Execution] (SAE) parameters.
//
// [Streaming Asynchronous Execution]: https://github.com/avalanche-foundation/ACPs/tree/main/ACPs/194-streaming-asynchronous-execution
package params

// Lambda is the denominator for computing the minimum gas consumed per
// transaction. For a transaction with gas limit `g`, the minimum consumption is
// ceil(g/Lambda).
const Lambda = 2
