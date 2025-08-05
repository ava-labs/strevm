package sae

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	vmpkg "github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/hookstest"
	"github.com/ava-labs/libevm/params"
	"github.com/ava-labs/strevm/hook"
	"github.com/stretchr/testify/require"
)

// shaPrecomputer implements both the SAE [hook.Points] pre-execution method and
// a [vmpkg.PrecompiledStatefulContract]. The hook filters transactions for
// those sent to a specified address, spawning a goroutine for each, while the
// precompile returns the pre-computed hashes from said goroutines.
type shaPrecomputer struct {
	precompile common.Address
	wg         sync.WaitGroup
	results    sync.Map
}

func (p *shaPrecomputer) BeforeExecutingBlock(b *types.Block) {
	for _, tx := range b.Transactions() {
		if to := tx.To(); to == nil || *to != p.precompile {
			continue
		}

		p.wg.Add(1)
		go func(tx *types.Transaction) {
			defer p.wg.Done()
			preimage := tx.Data()[:1] // In the PoC we know input length is non-zero
			p.results.Store(preimage[0], crypto.Keccak256(preimage))
		}(tx)
	}
}

var _ vmpkg.PrecompiledStatefulContract = (*shaPrecomputer)(nil).run

func (p *shaPrecomputer) run(env vmpkg.PrecompileEnvironment, input []byte) ([]byte, error) {
	p.wg.Wait() // This would be more fine-grained in practice, at tx resolution.
	out, _ := p.results.Load(input[0])
	env.StateDB().AddLog(&types.Log{
		Data: out.([]byte), // Quick & dirty for PoC only
	})
	return []byte{}, nil // Digest could be returned too, but easier to test with logs
}

type saeHooks struct {
	*shaPrecomputer
}

var _ hook.Points = saeHooks{}

func (saeHooks) GasTarget(*types.Block) gas.Gas { return 1e7 }

func TestExperimentalPrecomputingPrecompile(t *testing.T) {
	ctx := context.Background()

	precompileAddr := common.Address{0, 1, 2, 3}
	precompile := &shaPrecomputer{
		precompile: precompileAddr,
	}

	key := newTestPrivateKey(t, []byte{})
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	vm := newVM(
		ctx, t, time.Now,
		saeHooks{precompile}, // Registers [precomputer.BeforeExecutingBlock]
		logging.NoLog{},
		genesisJSON(t, 0, params.AllDevChainProtocolChanges, eoa),
	)
	t.Cleanup(func() {
		vm.Shutdown(ctx)
	})

	libevmHooks := &hookstest.Stub{
		PrecompileOverrides: map[common.Address]libevm.PrecompiledContract{
			precompileAddr: vmpkg.NewStatefulPrecompile(precompile.run),
		},
	}
	libevmHooks.Register(t)

	for i := range byte(2) {
		tx := types.MustSignNewTx(key, vm.currSigner(), &types.LegacyTx{
			Nonce:    uint64(i),
			To:       &precompileAddr,
			Data:     []byte{i},
			Gas:      1e6,
			GasPrice: big.NewInt(params.GWei),
		})
		require.NoError(t, vm.rpc.SendTransaction(ctx, tx))
	}

	b, err := vm.BuildBlock(ctx)
	require.NoError(t, err)
	require.NoError(t, vm.VerifyBlock(ctx, b))
	require.NoError(t, vm.AcceptBlock(ctx, b))
	require.NoError(t, b.WaitUntilExecuted(ctx))

	for i, r := range b.Receipts() {
		require.Len(t, r.Logs, 1)
		require.Equal(t, crypto.Keccak256([]byte{byte(i)}), r.Logs[0].Data)
	}
}
