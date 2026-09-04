package merkle

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func addr(b byte) common.Address {
	var a common.Address
	a[19] = b
	return a
}

func leaves(n int) []common.Hash {
	out := make([]common.Hash, n)
	for i := range out {
		out[i] = crypto.Keccak256Hash([]byte{byte(i), byte(i >> 8)})
	}
	return out
}

// TestProofsVerifyAtEverySize covers odd node counts specifically: those are where a
// tree that promotes an unpaired node and one that duplicates it diverge.
func TestProofsVerifyAtEverySize(t *testing.T) {
	for n := 1; n <= 33; n++ {
		ls := leaves(n)
		tree := Build(ls)
		root := tree.Root()

		if tree.Len() != n {
			t.Fatalf("n=%d: Len() = %d", n, tree.Len())
		}

		for i, leaf := range ls {
			proof, err := tree.Proof(i)
			if err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			if !Verify(root, leaf, proof) {
				t.Errorf("n=%d: proof for leaf %d did not verify", n, i)
			}
		}
	}
}

func TestVerifyRejectsForeignLeaf(t *testing.T) {
	ls := leaves(8)
	tree := Build(ls)
	proof, err := tree.Proof(3)
	if err != nil {
		t.Fatal(err)
	}

	foreign := crypto.Keccak256Hash([]byte("not in the tree"))
	if Verify(tree.Root(), foreign, proof) {
		t.Error("a leaf outside the tree verified against another leaf's proof")
	}
}

func TestSingleLeafTreeHasEmptyProof(t *testing.T) {
	only := leaves(1)
	tree := Build(only)

	if tree.Root() != only[0] {
		t.Errorf("root = %s, want the sole leaf %s", tree.Root().Hex(), only[0].Hex())
	}
	proof, err := tree.Proof(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 0 {
		t.Errorf("proof length = %d, want 0", len(proof))
	}
}

// TestEmptyTreeCommitsToZero guards the property the contract relies on: an empty
// commitment must not be able to "include" anything.
func TestEmptyTreeCommitsToZero(t *testing.T) {
	tree := Build(nil)
	if tree.Root() != (common.Hash{}) {
		t.Errorf("empty root = %s, want zero", tree.Root().Hex())
	}
	if _, err := tree.Proof(0); err == nil {
		t.Error("expected an error proving membership in an empty tree")
	}
	if Verify(common.Hash{}, crypto.Keccak256Hash([]byte("x")), nil) {
		t.Error("a leaf verified against the empty root")
	}
}

func TestAssetsHashIsOrderIndependentAndDeduplicated(t *testing.T) {
	a, b, c := addr(1), addr(2), addr(3)

	base := AssetsHash([]common.Address{a, b, c})
	if got := AssetsHash([]common.Address{c, a, b}); got != base {
		t.Errorf("reordering changed the digest: %s vs %s", got.Hex(), base.Hex())
	}
	if got := AssetsHash([]common.Address{b, a, c, a, b}); got != base {
		t.Errorf("duplicates changed the digest: %s vs %s", got.Hex(), base.Hex())
	}
	if got := AssetsHash([]common.Address{a, b}); got == base {
		t.Error("a different asset set produced the same digest")
	}
}

func TestAssetsHashDoesNotMutateInput(t *testing.T) {
	in := []common.Address{addr(3), addr(1), addr(2)}
	before := append([]common.Address(nil), in...)

	AssetsHash(in)

	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("AssetsHash reordered its argument: %v", in)
		}
	}
}

// TestAssetsHashMatchesPackedEncoding pins the wire format against the Solidity side,
// which hashes abi.encodePacked(address[]): 20 bytes per address, no padding.
func TestAssetsHashMatchesPackedEncoding(t *testing.T) {
	a, b := addr(1), addr(2)

	var packed []byte
	packed = append(packed, a.Bytes()...)
	packed = append(packed, b.Bytes()...)

	if got, want := AssetsHash([]common.Address{a, b}), crypto.Keccak256Hash(packed); got != want {
		t.Errorf("AssetsHash = %s, want keccak(packed) = %s", got.Hex(), want.Hex())
	}
}

// TestLeafHashMatchesAbiEncode pins the leaf format against HintRegistry.leafHash,
// which is keccak256(abi.encode(address, uint64, bytes32)) — three 32-byte words.
func TestLeafHashMatchesAbiEncode(t *testing.T) {
	account := common.HexToAddress("0xF4314B77F5A1dE41CAd89a9F1a80bce990A20F9a")
	var chainID uint64 = 1337
	assetsHash := crypto.Keccak256Hash([]byte("assets"))

	want := make([]byte, 96)
	copy(want[12:32], account.Bytes())
	// uint64 right-aligned in the second word, big-endian.
	for i := 0; i < 8; i++ {
		want[63-i] = byte(chainID >> (8 * i))
	}
	copy(want[64:96], assetsHash[:])

	if got := LeafHash(account, chainID, assetsHash); got != crypto.Keccak256Hash(want) {
		t.Errorf("LeafHash = %s, want %s", got.Hex(), crypto.Keccak256Hash(want).Hex())
	}
}

func TestHashPairIsCommutative(t *testing.T) {
	// The Solidity verifier sorts each pair, so proofs carry no direction bits and
	// the Go builder must sort identically.
	x := crypto.Keccak256Hash([]byte("x"))
	y := crypto.Keccak256Hash([]byte("y"))

	if hashPair(x, y) != hashPair(y, x) {
		t.Error("hashPair is not order independent")
	}
	if bytes.Equal(x[:], y[:]) {
		t.Skip("degenerate fixture")
	}
}

func TestProofForLooksLeavesUpByValue(t *testing.T) {
	ls := leaves(5)
	tree := Build(ls)

	proof, err := tree.ProofFor(ls[2])
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(tree.Root(), ls[2], proof) {
		t.Error("proof from ProofFor did not verify")
	}
	if _, err := tree.ProofFor(crypto.Keccak256Hash([]byte("absent"))); err != ErrUnknownLeaf {
		t.Errorf("err = %v, want ErrUnknownLeaf", err)
	}
}
