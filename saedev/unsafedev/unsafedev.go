// Package unsafedev provides properties of an S256 private key that MUST NOT be
// used outside of development, along with other config for the saedev binary.
package unsafedev

import (
	"crypto/ecdsa"
	"math/big"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/crypto"
)

const (
	// SeedPhrase is the mnemonic for computation of the private key returned by
	// [MustPrivateKey].
	SeedPhrase = "test test test test test test test test test test test junk"
	// DerivationPath is the derivation path for [Address].
	DerivationPath = "m/44'/60'/0'/0/0"
)

// Address is the account derived from [SeedPhrase] using [DerivationPath].
func Address() common.Address {
	return common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
}

// MustPrivateKey returns the private key from which [Address] is derived. It
// panics on error.
func MustPrivateKey() *ecdsa.PrivateKey {
	key, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if err != nil {
		panic(err)
	}
	return key
}

func init() {
	if crypto.PubkeyToAddress(MustPrivateKey().PublicKey) != Address() {
		panic("Broken package invariants between private key and address")
	}
}

// ChainID returns the chain ID of the saedev binary's chain.
func ChainID() *big.Int {
	return big.NewInt(9876)
}
