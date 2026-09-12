package hintreg

import (
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// A signature from the submitter has to recover to its own sender once the legacy
// v is undone, because that is exactly what ecrecover on the resolver will do.
func TestEOASignIsRecoverableWithLegacyV(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s := NewEOASubmitter(nil, key, 1, 0)

	digest := [32]byte(crypto.Keccak256([]byte("hello")))
	sig, err := s.Sign(digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		t.Fatalf("signature len %d, v %d; want 65 bytes with v in {27,28}", len(sig), sig[64])
	}
	raw := append([]byte{}, sig...)
	raw[64] -= 27
	pub, err := crypto.SigToPub(digest[:], raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := crypto.PubkeyToAddress(*pub); got != s.Sender() {
		t.Fatalf("recovered %s, want %s", got.Hex(), s.Sender().Hex())
	}
}
