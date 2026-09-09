// Package merkle builds the commitment that gets posted to HintRegistry.
//
// The encoding here is load-bearing: it has to match HintRegistry.leafHash and
// HintRegistry.verifyInclusion exactly, or published roots are unverifiable on-chain.
// The scheme is the conventional sorted-pair keccak tree, so proofs carry no
// direction bits.
package merkle

import (
	"bytes"
	"errors"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ErrUnknownLeaf is returned when a proof is requested for an absent account.
var ErrUnknownLeaf = errors.New("merkle: account not in tree")

// AssetsHash is keccak256 over the account's asset addresses, packed and ascending.
//
// Mirrors abi.encodePacked(address[]) on the Solidity side: 20 bytes per address with
// no padding. Sorting makes the digest independent of query order, and dedup keeps a
// repeated address from changing it.
func AssetsHash(assets []common.Address) common.Hash {
	sorted := make([]common.Address, len(assets))
	copy(sorted, assets)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	buf := make([]byte, 0, len(sorted)*common.AddressLength)
	var prev *common.Address
	for i := range sorted {
		if prev != nil && *prev == sorted[i] {
			continue
		}
		buf = append(buf, sorted[i][:]...)
		prev = &sorted[i]
	}
	return crypto.Keccak256Hash(buf)
}

// LeafHash mirrors HintRegistry.leafHash: keccak256(abi.encode(account, chainId, assetsHash)).
//
// abi.encode pads every value to a 32-byte word, so this is three words: the address
// right-aligned, the uint64 right-aligned, and the digest as-is.
func LeafHash(account common.Address, chainID uint64, assetsHash common.Hash) common.Hash {
	buf := make([]byte, 96)
	copy(buf[12:32], account[:])
	for i := 0; i < 8; i++ {
		buf[63-i] = byte(chainID >> (8 * i))
	}
	copy(buf[64:96], assetsHash[:])
	return crypto.Keccak256Hash(buf)
}

// hashPair combines two nodes in ascending order, matching the Solidity verifier.
func hashPair(a, b common.Hash) common.Hash {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return crypto.Keccak256Hash(a[:], b[:])
	}
	return crypto.Keccak256Hash(b[:], a[:])
}

// Tree is a built merkle tree retaining its levels so proofs can be served.
type Tree struct {
	levels [][]common.Hash // levels[0] is the leaves
	index  map[common.Hash]int
}

// Build constructs a tree over leaves in the given order.
//
// An odd node at a level is promoted unchanged to the next level rather than being
// paired with itself; pairing a node with itself lets a proof for an internal node be
// replayed as a proof for a leaf.
func Build(leaves []common.Hash) *Tree {
	t := &Tree{index: make(map[common.Hash]int, len(leaves))}
	if len(leaves) == 0 {
		return t
	}

	level := make([]common.Hash, len(leaves))
	copy(level, leaves)
	for i, l := range level {
		if _, seen := t.index[l]; !seen {
			t.index[l] = i
		}
	}
	t.levels = append(t.levels, level)

	for len(level) > 1 {
		next := make([]common.Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			next = append(next, hashPair(level[i], level[i+1]))
		}
		t.levels = append(t.levels, next)
		level = next
	}
	return t
}

// Root returns the tree root. An empty tree commits to the zero hash, which no leaf
// can produce, so an empty commitment cannot be claimed to include anything.
func (t *Tree) Root() common.Hash {
	if len(t.levels) == 0 {
		return common.Hash{}
	}
	top := t.levels[len(t.levels)-1]
	return top[0]
}

// Len reports the leaf count.
func (t *Tree) Len() int {
	if len(t.levels) == 0 {
		return 0
	}
	return len(t.levels[0])
}

// Proof returns the sibling path for the leaf at index i.
func (t *Tree) Proof(i int) ([]common.Hash, error) {
	if len(t.levels) == 0 || i < 0 || i >= len(t.levels[0]) {
		return nil, ErrUnknownLeaf
	}
	var proof []common.Hash
	idx := i
	for depth := 0; depth < len(t.levels)-1; depth++ {
		level := t.levels[depth]
		sibling := idx ^ 1
		// A promoted odd node has no sibling at this level.
		if sibling < len(level) {
			proof = append(proof, level[sibling])
		}
		idx /= 2
	}
	return proof, nil
}

// ProofFor returns the sibling path for a leaf by value.
func (t *Tree) ProofFor(leaf common.Hash) ([]common.Hash, error) {
	i, ok := t.index[leaf]
	if !ok {
		return nil, ErrUnknownLeaf
	}
	return t.Proof(i)
}

// Verify replays a proof. Kept here so tests can assert the Go and Solidity
// verifiers agree without a chain round-trip.
func Verify(root, leaf common.Hash, proof []common.Hash) bool {
	node := leaf
	for _, p := range proof {
		node = hashPair(node, p)
	}
	return node == root
}

// CoverageLeaf mirrors HintRegistry.coverageLeaf:
// keccak256(abi.encode(assetKey, fromBlock, toBlock)).
//
// One leaf per asset an epoch scanned, declaring the block range the publisher stands
// behind for it. The registry pays coverage rewards against these leaves, so the
// encoding is as load-bearing as LeafHash.
func CoverageLeaf(assetKey common.Hash, fromBlock, toBlock uint64) common.Hash {
	buf := make([]byte, 96)
	copy(buf[0:32], assetKey[:])
	for i := 0; i < 8; i++ {
		buf[63-i] = byte(fromBlock >> (8 * i))
		buf[95-i] = byte(toBlock >> (8 * i))
	}
	return crypto.Keccak256Hash(buf)
}
