// Package userstate defines portable, account-authorized wallet state.
package userstate

import (
	"bytes"
	"errors"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Trie is a binary sparse Merkle trie. All paths are 256 bits, MSB first.
// Node preimages are 00||key||value (leaf), 01||left||right (branch),
// and 02 (empty leaf). Empty subtrees are implicit and never persisted.
type Trie struct {
	Root  common.Hash
	Nodes map[common.Hash][]byte
	items []Pair
}
type Pair struct {
	Key   common.Hash
	Value []byte
}

var Empty [257]common.Hash

func init() {
	Empty[256] = crypto.Keccak256Hash([]byte{2})
	for d := 255; d >= 0; d-- {
		Empty[d] = branch(Empty[d+1], Empty[d+1])
	}
}
func branch(l, r common.Hash) common.Hash { return crypto.Keccak256Hash([]byte{1}, l[:], r[:]) }
func bit(k common.Hash, d int) byte       { return (k[d/8] >> (7 - uint(d%8))) & 1 }
func split(p []Pair, d int) int {
	return sort.Search(len(p), func(i int) bool { return bit(p[i].Key, d) == 1 })
}
func NewTrie(items []Pair) (*Trie, error) {
	p := append([]Pair(nil), items...)
	sort.Slice(p, func(i, j int) bool { return bytes.Compare(p[i].Key[:], p[j].Key[:]) < 0 })
	for i := range p {
		if len(p[i].Value) == 0 {
			return nil, errors.New("empty values are not entries")
		}
		if i > 0 && p[i].Key == p[i-1].Key {
			return nil, errors.New("duplicate key")
		}
		p[i].Value = bytes.Clone(p[i].Value)
	}
	t := &Trie{Nodes: make(map[common.Hash][]byte), items: p}
	t.Root = t.build(p, 0)
	return t, nil
}
func (t *Trie) build(p []Pair, d int) common.Hash {
	if len(p) == 0 {
		return Empty[d]
	}
	var raw []byte
	if d == 256 {
		raw = append(append([]byte{0}, p[0].Key[:]...), p[0].Value...)
	} else {
		i := split(p, d)
		l, r := t.build(p[:i], d+1), t.build(p[i:], d+1)
		raw = append(append([]byte{1}, l[:]...), r[:]...)
	}
	h := crypto.Keccak256Hash(raw)
	t.Nodes[h] = raw
	return h
}

// Proof carries siblings from root to leaf. Nil Value proves absence.
type Proof struct {
	Key      common.Hash   `json:"key"`
	Value    []byte        `json:"value"`
	Siblings []common.Hash `json:"siblings"`
}

func (t *Trie) Proof(key common.Hash) Proof {
	out := Proof{Key: key, Siblings: make([]common.Hash, 256)}
	h := t.Root
	for d := 0; d < 256; d++ {
		if h == Empty[d] {
			for j := d; j < 256; j++ {
				out.Siblings[j] = Empty[j+1]
			}
			return out
		}
		raw := t.Nodes[h]
		l, r := common.BytesToHash(raw[1:33]), common.BytesToHash(raw[33:])
		if bit(key, d) == 0 {
			out.Siblings[d] = r
			h = l
		} else {
			out.Siblings[d] = l
			h = r
		}
	}
	if h != Empty[256] {
		out.Value = bytes.Clone(t.Nodes[h][33:])
	}
	return out
}
func VerifyProof(root common.Hash, p Proof) bool {
	if len(p.Siblings) != 256 {
		return false
	}
	h := Empty[256]
	if len(p.Value) > 0 {
		h = crypto.Keccak256Hash([]byte{0}, p.Key[:], p.Value)
	}
	for d := 255; d >= 0; d-- {
		if bit(p.Key, d) == 0 {
			h = branch(h, p.Siblings[d])
		} else {
			h = branch(p.Siblings[d], h)
		}
	}
	return h == root
}
