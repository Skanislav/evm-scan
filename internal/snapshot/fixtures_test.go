package snapshot

// Cross-language test vectors for the TypeScript mirror in ../../mirror.
//
// The mirror recomputes this package's roots in the browser, so its keccak has to
// agree with Go's byte-for-byte — and a hash that disagrees produces a wrong root
// silently, never an error. Go is the side that already agrees with Solidity
// (cmd/evmscan-verify checks that against the real verifier), so Go generates and
// TypeScript is held to the output.
//
// Regenerate with:
//
//	go test ./internal/snapshot/ -run Fixtures -update-fixtures
//
// Without the flag the test asserts the committed files still match, so changing a
// hash on this side fails here rather than in a wallet.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/merkle"
)

var updateFixtures = flag.Bool("update-fixtures", false,
	"rewrite mirror/testdata from this package instead of asserting it matches")

const fixtureDir = "../../mirror/testdata"

// dec renders a uint64 as a decimal string. See the note on fixtureCase.ChainID.
func dec(v uint64) string { return strconv.FormatUint(v, 10) }

// fixtureCase is one whole snapshot document plus the roots Go builds from it.
type fixtureCase struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	File string `json:"file"`
	// ChainID and the block numbers are decimal strings, not JSON numbers: they
	// are uint64, and a reader on the other side of this file goes through
	// JSON.parse, which rounds anything above 2^53 before it can be checked.
	ChainID      string `json:"chain_id"`
	Leaves       int64  `json:"leaves"`
	Assets       int64  `json:"assets"`
	Root         string `json:"root"`
	CoverageRoot string `json:"coverage_root"`

	doc []byte // the NDJSON itself, written beside the manifest
}

// The cases exist to pin the edges, not to look like production data: an empty
// tree, a lone leaf, odd counts at more than one level, and a leaf whose assets
// arrive unsorted and duplicated.
func fixtureCases(t *testing.T) []fixtureCase {
	t.Helper()

	type spec struct {
		name     string
		why      string
		chainID  uint64
		coverage []Coverage
		leaves   []Leaf
	}

	specs := []spec{
		{
			name: "empty", chainID: 8453,
			why: "no rows: both roots are zero, which no leaf can produce",
		},
		{
			name: "single", chainID: 8453,
			why:      "one leaf is its own root, with an empty proof",
			coverage: []Coverage{{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200}},
			leaves:   []Leaf{{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}}},
		},
		{
			name: "even", chainID: 8453,
			why: "4 leaves: every level pairs cleanly",
			coverage: []Coverage{
				{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200},
				{Asset: addr(0xB2), FromBlock: 150, ToBlock: 200},
			},
			leaves: []Leaf{
				{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x02), Assets: []common.Address{addr(0xA1), addr(0xB2)}},
				{Account: addr(0x03), Assets: []common.Address{addr(0xB2)}},
				{Account: addr(0x04), Assets: []common.Address{addr(0xA1), addr(0xB2)}},
			},
		},
		{
			name: "odd", chainID: 8453,
			why: "3 leaves: the last is promoted unchanged, not paired with itself",
			coverage: []Coverage{
				{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200},
				{Asset: addr(0xB2), FromBlock: 150, ToBlock: 200},
				{Asset: addr(0xC3), FromBlock: 1, ToBlock: 200},
			},
			leaves: []Leaf{
				{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x02), Assets: []common.Address{addr(0xA1), addr(0xB2)}},
				{Account: addr(0x03), Assets: []common.Address{addr(0xC3)}},
			},
		},
		{
			name: "odd-deep", chainID: 8453,
			why: "5 leaves: promotion happens at two levels, one of them not the bottom",
			coverage: []Coverage{
				{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200},
			},
			leaves: []Leaf{
				{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x02), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x03), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x04), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x05), Assets: []common.Address{addr(0xA1)}},
			},
		},
		{
			name: "messy-assets", chainID: 8453,
			why: "assets arrive descending and duplicated: AssetsHash sorts and dedups, so " +
				"both leaves below share one assets hash and differ only by account",
			coverage: []Coverage{
				{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200},
				{Asset: addr(0xB2), FromBlock: 100, ToBlock: 200},
				{Asset: addr(0xC3), FromBlock: 100, ToBlock: 200},
			},
			leaves: []Leaf{
				{Account: addr(0x01), Assets: []common.Address{
					addr(0xC3), addr(0xA1), addr(0xB2), addr(0xA1)}},
				{Account: addr(0x02), Assets: []common.Address{
					addr(0xA1), addr(0xB2), addr(0xC3)}},
			},
		},
		{
			// chainId is one abi.encode word, and a mirror written with a 32-bit int
			// or a JS number would still pass every case above.
			name: "wide-chain-id", chainID: 0xFEDCBA9876543210,
			why:      "chain id fills all 8 bytes: catches a 32-bit or float encoding",
			coverage: []Coverage{{Asset: addr(0xA1), FromBlock: 0, ToBlock: 0xFFFFFFFFFF}},
			leaves: []Leaf{
				{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}},
				{Account: addr(0x02), Assets: []common.Address{addr(0xA1)}},
			},
		},
	}

	out := make([]fixtureCase, 0, len(specs))
	for _, sp := range specs {
		h := Header{
			ChainID:         sp.chainID,
			FromBlock:       1,
			ToBlock:         200,
			LeafCount:       int64(len(sp.leaves)),
			AssetCount:      int64(len(sp.coverage)),
			Registry:        addr(0xEE).Hex(),
			RegistryChainID: 8453,
		}
		res := Rebuild(h, sp.coverage, sp.leaves)
		h.Root = res.Root.Hex()
		h.CoverageRoot = res.CoverageRoot.Hex()

		var buf bytes.Buffer
		w, err := NewWriter(&buf, h)
		if err != nil {
			t.Fatalf("%s: new writer: %v", sp.name, err)
		}
		for _, c := range sp.coverage {
			if err := w.WriteCoverage(c); err != nil {
				t.Fatalf("%s: write coverage: %v", sp.name, err)
			}
		}
		for _, l := range sp.leaves {
			if err := w.WriteLeaf(l); err != nil {
				t.Fatalf("%s: write leaf: %v", sp.name, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: close: %v", sp.name, err)
		}

		out = append(out, fixtureCase{
			Name: sp.name, Why: sp.why, File: sp.name + ".ndjson",
			ChainID: dec(sp.chainID), Leaves: res.Leaves, Assets: res.Assets,
			Root: h.Root, CoverageRoot: h.CoverageRoot,
			doc: buf.Bytes(),
		})
	}
	return out
}

// The primitives, isolated. A whole-document case tells a mirror that something is
// wrong; these tell it which function.
type fixtureVectors struct {
	Note         string               `json:"note"`
	AssetsHash   []assetsHashVector   `json:"assets_hash"`
	LeafHash     []leafHashVector     `json:"leaf_hash"`
	AssetKey     []assetKeyVector     `json:"asset_key"`
	CoverageLeaf []coverageLeafVector `json:"coverage_leaf"`
	Root         []rootVector         `json:"root"`
}

type assetsHashVector struct {
	Why    string   `json:"why"`
	Assets []string `json:"assets"`
	Hash   string   `json:"hash"`
}

type leafHashVector struct {
	Why        string `json:"why"`
	Account    string `json:"account"`
	ChainID    string `json:"chain_id"`
	AssetsHash string `json:"assets_hash"`
	Leaf       string `json:"leaf"`
}

type assetKeyVector struct {
	ChainID string `json:"chain_id"`
	Asset   string `json:"asset"`
	Key     string `json:"key"`
}

type coverageLeafVector struct {
	AssetKey  string `json:"asset_key"`
	FromBlock string `json:"from_block"`
	ToBlock   string `json:"to_block"`
	Leaf      string `json:"leaf"`
}

// rootVector goes straight from leaf hashes to a root, so tree shape is tested
// without any leaf encoding in the way.
type rootVector struct {
	Why    string   `json:"why"`
	Leaves []string `json:"leaves"`
	Root   string   `json:"root"`
}

func buildVectors() fixtureVectors {
	hexes := func(as []common.Address) []string {
		out := make([]string, 0, len(as))
		for _, a := range as {
			out = append(out, a.Hex())
		}
		return out
	}
	assetsHash := func(as ...common.Address) assetsHashVector {
		return assetsHashVector{Assets: hexes(as), Hash: merkle.AssetsHash(as).Hex()}
	}

	v := fixtureVectors{
		Note: "generated by internal/snapshot/fixtures_test.go; regenerate with " +
			"go test ./internal/snapshot/ -run Fixtures -update-fixtures",
	}

	v.AssetsHash = []assetsHashVector{
		func() assetsHashVector {
			a := assetsHash()
			a.Why = "no assets: keccak of the empty string"
			return a
		}(),
		func() assetsHashVector {
			a := assetsHash(addr(0xA1))
			a.Why = "one asset, packed as 20 bytes with no padding"
			return a
		}(),
		func() assetsHashVector {
			a := assetsHash(addr(0xC3), addr(0xA1), addr(0xB2))
			a.Why = "descending input: sorted before packing"
			return a
		}(),
		func() assetsHashVector {
			a := assetsHash(addr(0xA1), addr(0xA1), addr(0xB2))
			a.Why = "repeated address: deduped, so it cannot change the digest"
			return a
		}(),
	}

	zero := common.Hash{}
	v.LeafHash = []leafHashVector{
		{
			Why:     "three abi.encode words: address right-aligned, uint64 right-aligned, digest as-is",
			Account: addr(0x01).Hex(), ChainID: dec(8453),
			AssetsHash: merkle.AssetsHash([]common.Address{addr(0xA1)}).Hex(),
			Leaf: merkle.LeafHash(addr(0x01), 8453,
				merkle.AssetsHash([]common.Address{addr(0xA1)})).Hex(),
		},
		{
			Why:     "chain id 0 and a zero digest: no field may be skipped",
			Account: common.Address{}.Hex(), ChainID: dec(0), AssetsHash: zero.Hex(),
			Leaf: merkle.LeafHash(common.Address{}, 0, zero).Hex(),
		},
		{
			Why:     "chain id filling all 8 bytes",
			Account: addr(0x02).Hex(), ChainID: dec(0xFEDCBA9876543210), AssetsHash: zero.Hex(),
			Leaf: merkle.LeafHash(addr(0x02), 0xFEDCBA9876543210, zero).Hex(),
		},
	}

	v.AssetKey = []assetKeyVector{
		{ChainID: dec(8453), Asset: addr(0xA1).Hex(), Key: AssetKey(8453, addr(0xA1)).Hex()},
		{ChainID: dec(1), Asset: addr(0xA1).Hex(), Key: AssetKey(1, addr(0xA1)).Hex()},
	}

	key := AssetKey(8453, addr(0xA1))
	v.CoverageLeaf = []coverageLeafVector{
		{AssetKey: key.Hex(), FromBlock: dec(100), ToBlock: dec(200),
			Leaf: merkle.CoverageLeaf(key, 100, 200).Hex()},
		{AssetKey: key.Hex(), FromBlock: dec(0), ToBlock: dec(0xFFFFFFFFFF),
			Leaf: merkle.CoverageLeaf(key, 0, 0xFFFFFFFFFF).Hex()},
	}

	// Leaf hashes chosen so the pair ordering matters: h(0x00..01) sorts below
	// h(0x00..02), and the sorted-pair rule has to put them back in that order
	// whichever way round they are handed in.
	leaf := func(n byte) common.Hash {
		var h common.Hash
		h[31] = n
		return h
	}
	rootOf := func(hs ...common.Hash) string { return merkle.Build(hs).Root().Hex() }
	hexHashes := func(hs []common.Hash) []string {
		out := make([]string, 0, len(hs))
		for _, h := range hs {
			out = append(out, h.Hex())
		}
		return out
	}

	for _, c := range []struct {
		why    string
		leaves []common.Hash
	}{
		{"one leaf is the root", []common.Hash{leaf(1)}},
		{"ascending pair", []common.Hash{leaf(1), leaf(2)}},
		{"descending pair: hashPair sorts, so this equals the ascending root",
			[]common.Hash{leaf(2), leaf(1)}},
		{"three leaves: the third is promoted, not doubled",
			[]common.Hash{leaf(1), leaf(2), leaf(3)}},
		{"five leaves: promotion at two levels",
			[]common.Hash{leaf(1), leaf(2), leaf(3), leaf(4), leaf(5)}},
		{"eight leaves: a full tree",
			[]common.Hash{leaf(1), leaf(2), leaf(3), leaf(4), leaf(5), leaf(6), leaf(7), leaf(8)}},
	} {
		v.Root = append(v.Root, rootVector{
			Why: c.why, Leaves: hexHashes(c.leaves), Root: rootOf(c.leaves...),
		})
	}
	return v
}

// TestMirrorFixtures keeps mirror/testdata in step with this package.
func TestMirrorFixtures(t *testing.T) {
	cases := fixtureCases(t)

	files := map[string][]byte{}
	for _, c := range cases {
		files[c.File] = c.doc
	}

	manifest, err := json.MarshalIndent(struct {
		Note   string        `json:"note"`
		Format string        `json:"format"`
		Cases  []fixtureCase `json:"cases"`
	}{
		Note: "generated by internal/snapshot/fixtures_test.go; regenerate with " +
			"go test ./internal/snapshot/ -run Fixtures -update-fixtures",
		Format: Format,
		Cases:  cases,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	files["manifest.json"] = append(manifest, '\n')

	vectors, err := json.MarshalIndent(buildVectors(), "", "  ")
	if err != nil {
		t.Fatalf("marshal vectors: %v", err)
	}
	files["vectors.json"] = append(vectors, '\n')

	if *updateFixtures {
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(fixtureDir, name), body, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		t.Logf("wrote %d files to %s", len(files), fixtureDir)
		return
	}

	for name, want := range files {
		path := filepath.Join(fixtureDir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v\n%s", path, err, regenerate)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: the TypeScript mirror is being tested against hashes "+
				"this package no longer produces.\n%s", path, regenerate)
		}
	}
}

const regenerate = "regenerate with: go test ./internal/snapshot/ -run Fixtures -update-fixtures"

// TestMirrorFixtureCasesAreSelfConsistent checks the generated documents the way a
// mirror will: rebuild from the rows and compare with the header. If this fails the
// fixtures are not a fair test of anything.
func TestMirrorFixtureCasesAreSelfConsistent(t *testing.T) {
	for _, c := range fixtureCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			res, err := Verify(bytes.NewReader(c.doc), nil)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if got := res.Root.Hex(); got != c.Root {
				t.Errorf("root %s, manifest says %s", got, c.Root)
			}
			if got := res.CoverageRoot.Hex(); got != c.CoverageRoot {
				t.Errorf("coverage root %s, manifest says %s", got, c.CoverageRoot)
			}
			if !res.Matches(common.HexToHash(c.Root), common.HexToHash(c.CoverageRoot)) {
				t.Error("Matches disagrees with the header it was generated from")
			}
		})
	}
}
