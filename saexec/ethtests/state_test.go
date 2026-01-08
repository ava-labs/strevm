// Copyright (C) 2025-2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ethtests

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/eth/tracers/logger"
)

func initMatcher(st *testMatcher) {
	// Long tests:
	st.slow(`^stAttackTest/ContractCreationSpam`)
	st.slow(`^stBadOpcode/badOpcodes`)
	st.slow(`^stPreCompiledContracts/modexp`)
	st.slow(`^stQuadraticComplexityTest/`)
	st.slow(`^stStaticCall/static_Call50000`)
	st.slow(`^stStaticCall/static_Return50000`)
	st.slow(`^stSystemOperationsTest/CallRecursiveBomb`)
	st.slow(`^stTransactionTest/Opcodes_TransactionInit`)
	// Very time consuming
	st.skipLoad(`^stTimeConsuming/`)
	st.skipLoad(`.*vmPerformance/loop.*`)
	// Uses 1GB RAM per tested fork
	st.skipLoad(`^stStaticCall/static_Call1MB`)

	// Broken tests:
	// EOF is not part of cancun
	st.skipLoad(`^stEOF/`)
}

func TestState(t *testing.T) {
	t.Parallel()

	st := new(testMatcher)
	initMatcher(st)
	for _, dir := range []string{
		filepath.Join(baseDir, "EIPTests", "StateTests"),
		stateTestDir,
	} {
		st.walk(t, dir, func(t *testing.T, name string, test *StateTest) {
			t.Helper()
			execStateTest(t, st, test)
		})
	}
}

// TestLegacyState tests some older tests, which were moved to the folder
// 'LegacyTests' for the Istanbul fork.
func TestLegacyState(t *testing.T) {
	st := new(testMatcher)
	initMatcher(st)
	st.walk(t, legacyStateTestDir, func(t *testing.T, name string, test *StateTest) {
		t.Helper()
		execStateTest(t, st, test)
	})
}

// TestExecutionSpecState runs the test fixtures from execution-spec-tests.
func TestExecutionSpecState(t *testing.T) {
	if !common.FileExist(executionSpecStateTestDir) {
		t.Skipf("directory %s does not exist", executionSpecStateTestDir)
	}
	st := new(testMatcher)

	var skippedTestRegexp []string
	// TODO(cey): We cannot run Pre-check tests
	// skip code maxsize tests
	skippedTestRegexp = append(skippedTestRegexp, `.*/test_initcode.py::test_contract_creating_tx\[.*-over_limit_.*\]`)
	// skip intrinsic gas tests
	skippedTestRegexp = append(skippedTestRegexp, `.*/test_initcode.py::TestContractCreationGasUsage::test_gas_usage\[.*-too_little_intrinsic_gas.*\]`)
	// skip blob txs as it's not relevant to Avalanche EVM chains
	skippedTestRegexp = append(skippedTestRegexp, `.*blob_txs.*`)

	st.walk(t, executionSpecStateTestDir, func(t *testing.T, name string, test *StateTest) {
		t.Helper()
		for _, skippedTestName := range skippedTestRegexp {
			if regexp.MustCompile(skippedTestName).MatchString(name) {
				t.Skipf("test %s skipped", name)
			}
		}
		execStateTest(t, st, test)
	})
}

func execStateTest(t *testing.T, st *testMatcher, test *StateTest) {
	t.Helper()
	for _, subtest := range test.Subtests() {
		subtest := subtest
		key := fmt.Sprintf("%s/%d", subtest.Fork, subtest.Index)

		t.Run(key+"/hash/trie", func(t *testing.T) {
			withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
				err := test.Run(t, subtest, vmconfig, false, rawdb.HashScheme, func(state *StateTestState) {})
				return st.checkFailure(t, err)
			})
		})
		t.Run(key+"/hash/snap", func(t *testing.T) {
			withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
				var snapErr error
				err := test.Run(t, subtest, vmconfig, true, rawdb.HashScheme, func(state *StateTestState) {
					if state.Snapshots != nil && state.StateDB != nil {
						if _, err := state.Snapshots.Journal(state.StateDB.IntermediateRoot(false)); err != nil {
							snapErr = err
						}
					}
				})
				if snapErr != nil {
					return snapErr
				}
				return st.checkFailure(t, err)
			})
		})
		// Context(cey): Path scheme is not supported yet
		// t.Run(key+"/path/trie", func(t *testing.T) {
		// 	withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
		// 		err := test.Run(t, subtest, vmconfig, false, rawdb.PathScheme, func(state *StateTestState) {})
		// 		return st.checkFailure(t, err)
		// 	})
		// })
		// t.Run(key+"/path/snap", func(t *testing.T) {
		// 	withTrace(t, test.gasLimit(subtest), func(vmconfig vm.Config) error {
		// 		var snapErr error
		// 		err := test.Run(t, subtest, vmconfig, true, rawdb.PathScheme, func(state *StateTestState) {
		// 			if state.Snapshots != nil && state.StateDB != nil {
		// 				if _, err := state.Snapshots.Journal(state.StateDB.IntermediateRoot(false)); err != nil {
		// 					snapErr = err
		// 				}
		// 			}
		// 		})
		// 		if snapErr != nil {
		// 			return snapErr
		// 		}
		// 		return st.checkFailure(t, err)
		// 	})
		// })
	}
}

// Transactions with gasLimit above this value will not get a VM trace on failure.
const traceErrorLimit = 400000

func withTrace(t *testing.T, gasLimit uint64, test func(vm.Config) error) {
	t.Helper()
	// Use config from command line arguments.
	config := vm.Config{}
	err := test(config)
	if err == nil {
		return
	}

	// Test failed, re-run with tracing enabled.
	t.Error(err)
	if gasLimit > traceErrorLimit {
		t.Log("gas limit too high for EVM trace")
		return
	}
	buf := new(bytes.Buffer)
	w := bufio.NewWriter(buf)
	config.Tracer = logger.NewJSONLogger(&logger.Config{}, w)
	err2 := test(config)
	if !reflect.DeepEqual(err, err2) {
		t.Errorf("different error for second run: %v", err2)
	}
	err = w.Flush()
	if err != nil {
		t.Errorf("failed to flush writer: %v", err)
	}
	if buf.Len() == 0 {
		t.Log("no EVM operation logs generated")
	} else {
		t.Log("EVM operation log:\n" + buf.String())
	}
	// t.Logf("EVM output: 0x%x", tracer.Output())
	// t.Logf("EVM error: %v", tracer.Error())
}
