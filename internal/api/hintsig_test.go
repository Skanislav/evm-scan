package api

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// A wallet signs the EIP-712 digest and returns v in {27, 28}; the verifier must
// recover the account from exactly that.
func TestHintSignatureRecovers(t *testing.T) {
	key, _ := crypto.GenerateKey()
	account := crypto.PubkeyToAddress(key.PublicKey)
	digest := crypto.Keccak256Hash([]byte("some .xorf bytes"))
	deadline := big.NewInt(1_800_000_000)

	h, err := hintDigestToSign(account, digest, deadline)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27 // as a wallet returns it

	got, err := recoverHintSigner(account, digest, deadline, sig)
	if err != nil || got != account {
		t.Fatalf("recovered %s, %v; want %s", got.Hex(), err, account.Hex())
	}
	// A different deadline is a different message: the same signature no longer
	// recovers to the account.
	if got, _ := recoverHintSigner(account, digest, big.NewInt(1_800_000_001), sig); got == account {
		t.Fatal("a signature over another deadline recovered to the account")
	}
}

func TestHintSignatureRejectsOtherSigner(t *testing.T) {
	key, _ := crypto.GenerateKey()
	account := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	digest := crypto.Keccak256Hash([]byte("x"))
	deadline := big.NewInt(1)
	h, _ := hintDigestToSign(account, digest, deadline)
	sig, _ := crypto.Sign(h, key)
	sig[64] += 27
	got, err := recoverHintSigner(account, digest, deadline, sig)
	if err != nil {
		t.Fatal(err)
	}
	if got == account {
		t.Fatal("recovered the account from a stranger's signature")
	}
	if _, err := recoverHintSigner(account, digest, deadline, sig[:64]); err == nil {
		t.Fatal("a 64-byte signature was accepted")
	}
}
