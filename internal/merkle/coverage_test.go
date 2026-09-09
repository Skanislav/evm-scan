package merkle

import (
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The coverage leaf must be keccak256(abi.encode(bytes32, uint64, uint64)) exactly,
// so check the hand-rolled encoding against go-ethereum's ABI encoder.
func TestCoverageLeafMatchesABIEncode(t *testing.T) {
	b32, _ := abi.NewType("bytes32", "", nil)
	u64, _ := abi.NewType("uint64", "", nil)
	args := abi.Arguments{{Type: b32}, {Type: u64}, {Type: u64}}

	key := common.HexToHash("0x1234abcd")
	for _, tc := range [][2]uint64{{0, 0}, {1, 1}, {12, 18}, {1 << 40, 1<<63 + 5}} {
		packed, err := args.Pack([32]byte(key), tc[0], tc[1])
		if err != nil {
			t.Fatal(err)
		}
		want := crypto.Keccak256Hash(packed)
		if got := CoverageLeaf(key, tc[0], tc[1]); got != want {
			t.Fatalf("CoverageLeaf(%d,%d) = %s, want %s", tc[0], tc[1], got.Hex(), want.Hex())
		}
	}
}
