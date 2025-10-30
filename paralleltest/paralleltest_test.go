package paralleltest

import (
	"math/big"
	"slices"
	"testing"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm"
	"github.com/ava-labs/libevm/libevm/precompiles/parallel"
	"github.com/ava-labs/libevm/params"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"

	"github.com/ava-labs/strevm/blocks"
	"github.com/ava-labs/strevm/saetest"
)

func TestMain(m *testing.M) {
	saetest.NoLeak(m)
}

type handler struct {
	addr common.Address
	gas  uint64
}

var _ parallel.Handler[*txHashEchoer] = (*handler)(nil)

func (*handler) BeforeBlock(libevm.StateReader, *types.Block) {}

func (h *handler) Gas(tx *types.Transaction) (uint64, bool) {
	if to := tx.To(); to == nil || *to != h.addr {
		return 0, false
	}
	return h.gas, true
}

func (*handler) Process(_ libevm.StateReader, _ int, tx *types.Transaction) *txHashEchoer {
	return &txHashEchoer{hash: tx.Hash()}
}

func (*handler) AfterBlock(parallel.StateDB, *types.Block, types.Receipts) {}

type txHashEchoer struct {
	hash common.Hash
}

func shouldRevert(h common.Hash) bool {
	return h[0]%2 == 0
}

func (e *txHashEchoer) Revert() bool {
	return shouldRevert(e.hash)
}

func logs(h common.Hash) []*types.Log {
	return []*types.Log{{
		Topics: []common.Hash{h},
	}}
}

func (e *txHashEchoer) Logs() []*types.Log {
	return logs(e.hash)
}

func (e *txHashEchoer) ReturnData() []byte {
	return slices.Clone(e.hash.Bytes())
}

func TestNewExecutor(t *testing.T) {
	logger, ctx := saetest.NewTBLoggerAndContext(t.Context(), t, logging.Warn)

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	eoa := crypto.PubkeyToAddress(key.PublicKey)

	precompileAddr := common.Address{'p', 'r', 'e'}
	const precompileGas = 1000
	h := &handler{precompileAddr, precompileGas}
	config := params.MergedTestChainConfig

	exec := NewExecutor(
		t, logger,
		rawdb.NewMemoryDatabase(),
		config,
		saetest.MaxAllocFor(eoa),
		precompileAddr,
		h, 1,
	)

	last := exec.LastExecuted()
	signer := types.LatestSigner(config)
	var (
		chain []*blocks.Block
		nonce uint64
		txs   []common.Hash
	)

	for range 10 {
		tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			To:       &precompileAddr,
			GasPrice: big.NewInt(1),
			Gas:      params.TxGas + precompileGas,
		})
		nonce++
		txs = append(txs, tx.Hash())

		b, err := blocks.New(
			types.NewBlock(
				&types.Header{
					Number:     new(big.Int).Add(last.Number(), big.NewInt(1)),
					ParentHash: last.Hash(),
				},
				types.Transactions{tx},
				nil, nil, saetest.TrieHasher(),
			),
			last, nil,
			logger,
		)
		require.NoError(t, err)
		require.NoError(t, exec.Enqueue(ctx, b))

		chain = append(chain, b)
		last = b
	}

	require.NoError(t, last.WaitUntilExecuted(ctx))

	for i, b := range chain {
		require.Len(t, b.Receipts(), 1)

		ignore := cmp.Options{
			cmpopts.IgnoreFields(
				types.Receipt{},
				"BlockNumber", "BlockHash",
				"GasUsed", "CumulativeGasUsed",
				"Bloom",
			),
			cmpopts.IgnoreFields(
				types.Log{},
				"BlockNumber", "TxHash", "BlockHash",
			),
		}
		want := &types.Receipt{
			TxHash: txs[i],
		}
		if shouldRevert(want.TxHash) {
			want.Status = types.ReceiptStatusFailed
		} else {
			want.Status = types.ReceiptStatusSuccessful
			want.Logs = logs(txs[i])
		}

		if diff := cmp.Diff(want, b.Receipts()[0], ignore); diff != "" {
			t.Errorf("%s", diff)
		}
	}
}
