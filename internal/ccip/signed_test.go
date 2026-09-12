package ccip

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The hash is what the contract recomputes; a drift here is a signature the resolver
// rejects, which is why the vector is pinned rather than derived twice.
func TestSignedResponseHashVector(t *testing.T) {
	resolver := common.HexToAddress("0x1111111111111111111111111111111111111111")
	request := []byte("request")
	result := []byte("result")
	got := SignedResponseHash(resolver, 1_700_000_000, request, result)

	var exp [8]byte
	exp[4], exp[5], exp[6], exp[7] = 0x65, 0x53, 0xf1, 0x00 // 1_700_000_000 big-endian
	want := crypto.Keccak256(
		[]byte{0x19, 0x00}, resolver[:], exp[:], crypto.Keccak256(request), crypto.Keccak256(result),
	)
	if !bytes.Equal(got[:], want) {
		t.Fatalf("hash = %s, want %s", hex.EncodeToString(got[:]), hex.EncodeToString(want))
	}
	// Pinned once, from this implementation, so a change to the layout fails here
	// before it fails against a deployed resolver.
	const pinned = "d8b596769838d09f7d16ce6ba84bd350c4bf5dc62c634ff567c8aa73313dc49c"
	if hex.EncodeToString(got[:]) != pinned {
		t.Fatalf("vector drifted: %s, pinned %s", hex.EncodeToString(got[:]), pinned)
	}
}

func TestSignedResponseRoundTrip(t *testing.T) {
	sig := bytes.Repeat([]byte{0xab}, 65)
	enc, err := EncodeSignedResponse([]byte("abc"), 42, sig)
	if err != nil {
		t.Fatal(err)
	}
	result, expires, got, err := DecodeSignedResponse(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "abc" || expires != 42 || !bytes.Equal(got, sig) {
		t.Fatalf("round trip = %q %d %x", result, expires, got)
	}
	if _, err := EncodeSignedResponse(nil, 1, sig[:64]); err == nil {
		t.Fatal("a 64-byte signature was accepted")
	}
}
