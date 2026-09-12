package ccip

import (
	"encoding/binary"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// A signed gateway answer, as HintSignedResolver.resolveWithProof consumes it.
//
// This is the second wire shape a gateway here can produce. contractsOf's answer
// carries a merkle proof and the registry verifies it; this one carries a signature
// and the resolver checks who made it. The difference in what a reader learns is
// the whole point of keeping them apart: a proof means the root commits to the
// answer, a signature means the publisher said so recently.

// MethodResolveWithProof is the resolver's callback for a signed answer.
const MethodResolveWithProof = "resolveWithProof"

var signedResponseArgs = mustArgs("bytes", "uint64", "bytes")

// SignedResponseHash is what the gateway signs and the resolver recovers:
//
//	keccak256(0x1900 ‖ resolver ‖ expires ‖ keccak256(request) ‖ keccak256(result))
//
// Binding the resolver's address keeps a signature from meaning anything at another
// resolver; binding the request keeps an answer from being replayed for a different
// name or key. It is signed raw, never through an EIP-191 text prefix: the 0x1900
// prefix already makes it a validator-specific message that cannot collide with a
// transaction or a personal_sign payload.
func SignedResponseHash(resolver common.Address, expires uint64, request, result []byte) [32]byte {
	var exp [8]byte
	binary.BigEndian.PutUint64(exp[:], expires)
	return [32]byte(crypto.Keccak256(
		[]byte{0x19, 0x00},
		resolver[:],
		exp[:],
		crypto.Keccak256(request),
		crypto.Keccak256(result),
	))
}

// EncodeSignedResponse builds abi.encode(result, expires, signature).
func EncodeSignedResponse(result []byte, expires uint64, sig []byte) ([]byte, error) {
	if len(sig) != 65 {
		return nil, errors.New("ccip: signature must be 65 bytes")
	}
	return signedResponseArgs.Pack(result, expires, sig)
}

// DecodeSignedResponse is the inverse of EncodeSignedResponse.
func DecodeSignedResponse(b []byte) (result []byte, expires uint64, sig []byte, err error) {
	vals, err := signedResponseArgs.Unpack(b)
	if err != nil {
		return nil, 0, nil, err
	}
	result, _ = vals[0].([]byte)
	expires, _ = vals[1].(uint64)
	sig, _ = vals[2].([]byte)
	return result, expires, sig, nil
}
