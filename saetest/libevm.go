package saetest

import (
	"testing"

	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/strevm/hook"
)

// EnableMinimumGasConsumption registers [hook.MinimumGasConsumption] with a
// libevm [hookstest.Stub], clearing the registration with [testing.TB.Cleanup].
func EnableMinimumGasConsumption(tb testing.TB) {
	stub := hookstest.Stub{
		MinimumGasConsumptionFn: hook.MinimumGasConsumption,
	}
	stub.Register(tb)
}
