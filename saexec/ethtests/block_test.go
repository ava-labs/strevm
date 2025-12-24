// Copyright (C) 2019-2025, Ava Labs, Inc. All rights reserved.
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
	"math/rand"
	"regexp"
	"runtime"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
)

func TestBlockchain(t *testing.T) {
	bt := new(testMatcher)
	// General state tests are 'exported' as blockchain tests, but we can run them natively.
	// For speedier CI-runs, the line below can be uncommented, so those are skipped.
	// For now, in hardfork-times (Berlin), we run the tests both as StateTests and
	// as blockchain tests, since the latter also covers things like receipt root
	bt.slow(`^GeneralStateTests/`)

	// Skip random failures due to selfish mining test
	bt.skipLoad(`.*bcForgedTest/bcForkUncle\.json`)

	// Slow tests
	bt.slow(`.*bcExploitTest/DelegateCallSpam.json`)
	bt.slow(`.*bcExploitTest/ShanghaiLove.json`)
	bt.slow(`.*bcExploitTest/SuicideIssue.json`)
	bt.slow(`.*/bcForkStressTest/`)
	bt.slow(`.*/bcGasPricerTest/RPC_API_Test.json`)
	bt.slow(`.*/bcWalletTest/`)

	// Very slow test
	bt.skipLoad(`.*/stTimeConsuming/.*`)
	// test takes a lot for time and goes easily OOM because of sha3 calculation on a huge range,
	// using 4.6 TGas
	bt.skipLoad(`.*randomStatetest94.json.*`)

	// TODO(cey): We cannot run invalid blocks, since we don't currently have pre-insert checks.
	bt.skipLoad(`^InvalidBlocks/`)

	// TODO(cey): We cannot run Fork tests or side-chain tests as we don't support reorg/sidechain management?
	bt.skipLoad(`.*/bcForkStressTest/`)
	bt.skipLoad(`.*/bcTotalDifficultyTest/`)
	bt.skipLoad(`.*/bcMultiChainTest/`)
	bt.skipLoad(`.*/bcGasPricerTest/RPC_API_Test.json`)
	bt.skipLoad(`.*/bcFrontierToHomestead/blockChainFrontierWithLargerTDvsHomesteadBlockchain.json`)
	bt.skipLoad(`.*/bcFrontierToHomestead/blockChainFrontierWithLargerTDvsHomesteadBlockchain2.json`)
	var skippedTestRegexp []string
	// TODO(cey): We cannot run Pre-check tests
	// skip code maxsize tests
	skippedTestRegexp = append(skippedTestRegexp, `.*/contract_creating_tx.json/002-fork=Shanghai-over_limit_zeros-over_limit_zeros`)
	skippedTestRegexp = append(skippedTestRegexp, `.*/contract_creating_tx.json/003-fork=Shanghai-over_limit_ones-over_limit_ones`)
	skippedTestRegexp = append(skippedTestRegexp, `.*/contract_creating_tx.json/006-fork=Cancun-over_limit_zeros-over_limit_zeros`)
	skippedTestRegexp = append(skippedTestRegexp, `.*/contract_creating_tx.json/007-fork=Cancun-over_limit_ones-over_limit_ones`)
	// skip intrinsic gas tests
	skippedTestRegexp = append(skippedTestRegexp, `.*/gas_usage.json/.*too_little_intrinsic_gas.*`)
	bt.skipLoad(`.*/bcFrontierToHomestead/HomesteadOverrideFrontier.json`)
	bt.skipLoad(`.*/bcFrontierToHomestead/ContractCreationFailsOnHomestead.json`)
	// skip insufficient funds tests
	skippedTestRegexp = append(skippedTestRegexp, `.*/use_value_in_tx.json/000-fork=Shanghai-tx_in_withdrawals_block`)
	skippedTestRegexp = append(skippedTestRegexp, `.*/use_value_in_tx.json/002-fork=Cancun-tx_in_withdrawals_block`)
	// Skip tx type check
	bt.skipLoad(`.*/bcBerlinToLondon/initialVal.json`)

	bt.walk(t, blockTestDir, func(t *testing.T, name string, test *BlockTest) {
		if runtime.GOARCH == "386" && runtime.GOOS == "windows" && rand.Int63()%2 == 0 {
			t.Skip("test (randomly) skipped on 32-bit windows")
		}
		for _, skippedTestName := range skippedTestRegexp {
			if regexp.MustCompile(skippedTestName).MatchString(name) {
				t.Skipf("test %s skipped", name)
			}
		}
		execBlockTest(t, bt, test)
	})
	// There is also a LegacyTests folder, containing blockchain tests generated
	// prior to Istanbul. However, they are all derived from GeneralStateTests,
	// which run natively, so there's no reason to run them here.
	bt.walk(t, legacyBlockTestDir, func(t *testing.T, name string, test *BlockTest) {
		for _, skippedTestName := range skippedTestRegexp {
			if regexp.MustCompile(skippedTestName).MatchString(name) {
				t.Skipf("test %s skipped", name)
			}
		}
		execBlockTest(t, bt, test)
	})
}

// TestExecutionSpecBlocktests runs the test fixtures from execution-spec-tests.
func TestExecutionSpecBlocktests(t *testing.T) {
	if !common.FileExist(executionSpecBlockchainTestDir) {
		t.Skipf("directory %s does not exist", executionSpecBlockchainTestDir)
	}
	bt := new(testMatcher)

	bt.walk(t, executionSpecBlockchainTestDir, func(t *testing.T, name string, test *BlockTest) {
		execBlockTest(t, bt, test)
	})
}

func execBlockTest(t *testing.T, bt *testMatcher, test *BlockTest) {
	if err := bt.checkFailure(t, test.Run(t, false, rawdb.HashScheme, nil, nil)); err != nil {
		t.Errorf("test in hash mode without snapshotter failed: %v", err)
		return
	}
	if err := bt.checkFailure(t, test.Run(t, true, rawdb.HashScheme, nil, nil)); err != nil {
		t.Errorf("test in hash mode with snapshotter failed: %v", err)
		return
	}
	if err := bt.checkFailure(t, test.Run(t, false, rawdb.PathScheme, nil, nil)); err != nil {
		t.Errorf("test in path mode without snapshotter failed: %v", err)
		return
	}
	if err := bt.checkFailure(t, test.Run(t, true, rawdb.PathScheme, nil, nil)); err != nil {
		t.Errorf("test in path mode with snapshotter failed: %v", err)
		return
	}
}
