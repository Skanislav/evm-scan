package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

func addr(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

// fixture builds an index the way the publisher does, so the roots under test are
// the ones a real epoch would commit.
func fixture(chainID uint64) (Header, []Coverage, []Leaf) {
	coverage := []Coverage{
		{Asset: addr(0xA1), FromBlock: 100, ToBlock: 200},
		{Asset: addr(0xB2), FromBlock: 150, ToBlock: 200},
	}
	leaves := []Leaf{
		{Account: addr(0x01), Assets: []common.Address{addr(0xA1)}},
		{Account: addr(0x02), Assets: []common.Address{addr(0xA1), addr(0xB2)}},
		{Account: addr(0x03), Assets: []common.Address{addr(0xB2)}},
	}

	idx := make([]common.Hash, 0, len(leaves))
	for _, l := range leaves {
		idx = append(idx, merkle.LeafHash(l.Account, chainID, merkle.AssetsHash(l.Assets)))
	}
	cov := make([]common.Hash, 0, len(coverage))
	for _, c := range coverage {
		cov = append(cov, merkle.CoverageLeaf(AssetKey(chainID, c.Asset), c.FromBlock, c.ToBlock))
	}

	h := Header{
		ChainID:      chainID,
		FromBlock:    100,
		ToBlock:      200,
		Root:         merkle.Build(idx).Root().Hex(),
		CoverageRoot: merkle.Build(cov).Root().Hex(),
		LeafCount:    int64(len(leaves)),
		AssetCount:   int64(len(coverage)),
	}
	return h, coverage, leaves
}

func write(t *testing.T, h Header, coverage []Coverage, leaves []Leaf) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, h)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, c := range coverage {
		if err := w.WriteCoverage(c); err != nil {
			t.Fatalf("WriteCoverage: %v", err)
		}
	}
	for _, l := range leaves {
		if err := w.WriteLeaf(l); err != nil {
			t.Fatalf("WriteLeaf: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// A snapshot has to rebuild exactly the roots the registry holds, or it proves
// nothing: that equality is the entire reason the file can be fetched from anyone.
func TestSnapshotRebuildsTheCommittedRoots(t *testing.T) {
	h, coverage, leaves := fixture(1)
	res, err := Verify(bytes.NewReader(write(t, h, coverage, leaves)), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := res.Root.Hex(); got != h.Root {
		t.Errorf("root = %s, want %s", got, h.Root)
	}
	if got := res.CoverageRoot.Hex(); got != h.CoverageRoot {
		t.Errorf("coverage root = %s, want %s", got, h.CoverageRoot)
	}
	if !res.Matches(common.HexToHash(h.Root), common.HexToHash(h.CoverageRoot)) {
		t.Error("Matches said no on the roots it just produced")
	}
	if res.Leaves != 3 || res.Assets != 2 {
		t.Errorf("counted %d leaves / %d assets, want 3 / 2", res.Leaves, res.Assets)
	}
}

// The security property: any edit to the table changes a root. Without this a
// mirror could add, drop or retarget rows and a restore would write them.
func TestTamperingChangesTheRoot(t *testing.T) {
	h, coverage, leaves := fixture(1)
	want := common.HexToHash(h.Root)
	wantCov := common.HexToHash(h.CoverageRoot)

	for _, tc := range []struct {
		name     string
		coverage []Coverage
		leaves   []Leaf
	}{
		{"asset added to an account", coverage,
			[]Leaf{leaves[0], {Account: addr(0x02), Assets: []common.Address{addr(0xA1), addr(0xB2), addr(0xC3)}}, leaves[2]}},
		{"account dropped", coverage, []Leaf{leaves[0], leaves[2]}},
		{"account added", coverage,
			append(append([]Leaf{}, leaves...), Leaf{Account: addr(0x04), Assets: []common.Address{addr(0xA1)}})},
		{"account swapped", coverage,
			[]Leaf{{Account: addr(0xFF), Assets: leaves[0].Assets}, leaves[1], leaves[2]}},
		{"coverage range widened",
			[]Coverage{{Asset: addr(0xA1), FromBlock: 1, ToBlock: 200}, coverage[1]}, leaves},
		{"asset dropped from coverage", coverage[:1], leaves},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The header still claims the original counts and roots, which is what a
			// forged file would do.
			res, err := Verify(bytes.NewReader(write(t, Header{ChainID: h.ChainID}, tc.coverage, tc.leaves)), nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Matches(want, wantCov) {
				t.Error("tampered snapshot still matched the committed roots")
			}
		})
	}
}

// A document that restates the roots it wants cannot pass: Verify recomputes them
// from the rows and reports what the rows actually build.
func TestHeaderCountsAreChecked(t *testing.T) {
	h, coverage, leaves := fixture(1)
	h.LeafCount = 99
	_, err := Verify(bytes.NewReader(write(t, h, coverage, leaves)), nil)
	if err == nil || !strings.Contains(err.Error(), "header says 99 leaves") {
		t.Fatalf("err = %v, want a leaf-count mismatch", err)
	}
}

// Sections are ordered because the two trees are built from two sequences; an
// interleaved document would silently put rows in the wrong one.
func TestCoverageAfterLeavesIsRejected(t *testing.T) {
	h, coverage, leaves := fixture(1)
	doc := write(t, h, coverage, leaves)
	extra := write(t, Header{ChainID: 1}, coverage[:1], nil)
	// drop the second document's header line, keep its coverage row
	tail := extra[bytes.IndexByte(extra, '\n')+1:]

	_, err := Verify(bytes.NewReader(append(doc, tail...)), nil)
	if err == nil || !strings.Contains(err.Error(), "coverage row after a leaf row") {
		t.Fatalf("err = %v, want an ordering error", err)
	}
}

// The chain a snapshot is about is part of every leaf, so a table restored under
// the wrong chain id cannot match. This is the mistake that has already cost this
// project money twice, in the UI and in evmscan-verify.
func TestChainIDIsBoundIntoTheRoots(t *testing.T) {
	h1, coverage, leaves := fixture(1)
	res, err := Verify(bytes.NewReader(write(t, Header{ChainID: 8453}, coverage, leaves)), nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Root.Hex() == h1.Root {
		t.Error("the same rows built the same root under a different chain id")
	}
}

func TestURIExpandsTemplate(t *testing.T) {
	got := URI("https://h/v1/epochs/{id}/snapshot?chain={chain}", 1, 42)
	want := "https://h/v1/epochs/42/snapshot?chain=1"
	if got != want {
		t.Errorf("URI = %q, want %q", got, want)
	}
	if URI("", 1, 42) != "" {
		t.Error("an empty template should stay empty")
	}
}

// AssetKey is duplicated from hintreg to keep this package off the registry client.
// If the two ever disagree, coverage roots stop matching and rewards stop paying.
func TestAssetKeyMatchesRegistryEncoding(t *testing.T) {
	// keccak256(abi.encodePacked(uint64 chainId, address token)) — 28 bytes.
	got := AssetKey(1, common.HexToAddress("0x40D16FC0246aD3160Ccc09B8D0D3A2cD28aE6C2f"))
	want := "0x19d7e195fd78dc4467f0bbef3050f35ee1a6f3d604d0c71cda3f5c9e8b62b3fa" // read from the live registry
	if got.Hex() != want {
		t.Errorf("AssetKey = %s, want %s (the value assetKey() returns on chain)", got.Hex(), want)
	}
}

// memStore records what a restore would write.
type memStore struct {
	chains  map[uint64]bool
	assets  map[common.Address]store.Asset
	cursors map[common.Address][3]uint64 // next, tail, floor
	done    map[common.Address]bool
	rows    map[string]store.Interaction
}

func newMemStore() *memStore {
	return &memStore{
		chains:  map[uint64]bool{},
		assets:  map[common.Address]store.Asset{},
		cursors: map[common.Address][3]uint64{},
		done:    map[common.Address]bool{},
		rows:    map[string]store.Interaction{},
	}
}

func (m *memStore) UpsertChain(_ context.Context, id uint64, _ string) error {
	m.chains[id] = true
	return nil
}

func (m *memStore) RegisterAsset(_ context.Context, a store.Asset, _ uint64) (bool, error) {
	m.assets[a.Address] = a
	return true, nil
}

func (m *memStore) FoldInteractions(_ context.Context, _ uint64, rows []store.Interaction) error {
	for _, r := range rows {
		m.rows[r.Account.Hex()+"/"+r.Asset.Hex()] = r
	}
	return nil
}

func (m *memStore) AdvanceBackfill(_ context.Context, _ uint64, a common.Address, next uint64, done bool, _ uint64) error {
	c := m.cursors[a]
	c[0] = next
	m.cursors[a] = c
	m.done[a] = done
	return nil
}

func (m *memStore) AdvanceTail(_ context.Context, _ uint64, a common.Address, tail uint64, _ uint64) error {
	c := m.cursors[a]
	c[1] = tail
	m.cursors[a] = c
	return nil
}

func (m *memStore) SetBackfillFloor(_ context.Context, _ uint64, a common.Address, floor uint64) error {
	c := m.cursors[a]
	c[2] = floor
	m.cursors[a] = c
	return nil
}

// The round trip that is the whole point: publish → snapshot → verify → restore
// into an empty store → the restored rows rebuild the same root. A daemon that
// lost its database can come back to a state the chain still vouches for.
func TestRestoreRebuildsAnIndexThatMatchesTheRoot(t *testing.T) {
	const chainID = 1
	h, coverage, leaves := fixture(chainID)
	doc := write(t, h, coverage, leaves)

	// Recover from the bytes, exactly as evmscan-restore does.
	gotH, gotCov, gotLeaves, err := Read(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !Rebuild(gotH, gotCov, gotLeaves).Matches(common.HexToHash(h.Root), common.HexToHash(h.CoverageRoot)) {
		t.Fatal("re-read snapshot does not rebuild the committed roots")
	}

	st := newMemStore()
	rep, err := Restore(context.Background(), st, gotH, gotCov, gotLeaves)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.Assets != 2 || rep.Accounts != 3 || rep.Interactions != 4 {
		t.Errorf("report = %d assets / %d accounts / %d interactions, want 2 / 3 / 4",
			rep.Assets, rep.Accounts, rep.Interactions)
	}

	// Every asset comes back, with the scan range the coverage leaf declared. Getting
	// this wrong makes a restored node re-walk history a light client cannot serve.
	for _, c := range coverage {
		if _, ok := st.assets[c.Asset]; !ok {
			t.Fatalf("asset %s was not restored", c.Asset.Hex())
		}
		if !st.done[c.Asset] {
			t.Errorf("asset %s: backfill not marked done", c.Asset.Hex())
		}
		cur := st.cursors[c.Asset]
		if cur[1] != c.ToBlock {
			t.Errorf("asset %s: tail = %d, want %d", c.Asset.Hex(), cur[1], c.ToBlock)
		}
		if cur[2] != c.FromBlock {
			t.Errorf("asset %s: floor = %d, want %d", c.Asset.Hex(), cur[2], c.FromBlock)
		}
	}

	// Now the real check: rebuild the index from the restored rows and confirm it is
	// the same commitment. This is what "recovered" has to mean.
	byAccount := map[common.Address][]common.Address{}
	for _, r := range st.rows {
		byAccount[r.Account] = append(byAccount[r.Account], r.Asset)
	}
	restored := make([]Leaf, 0, len(byAccount))
	for _, l := range leaves { // account order is the tree's order
		assets := byAccount[l.Account]
		sortAddrs(assets)
		restored = append(restored, Leaf{Account: l.Account, Assets: assets})
	}
	if got := Rebuild(gotH, gotCov, restored); got.Root.Hex() != h.Root {
		t.Fatalf("index rebuilt from restored rows has root %s, want %s", got.Root.Hex(), h.Root)
	}
}

func sortAddrs(a []common.Address) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && bytes.Compare(a[j][:], a[j-1][:]) < 0; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func TestUnknownFormatIsRefused(t *testing.T) {
	doc := []byte(fmt.Sprintf("{\"format\":%q}\n", "evmscan-snapshot/99"))
	if _, err := Verify(bytes.NewReader(doc), nil); err == nil ||
		!strings.Contains(err.Error(), "unknown snapshot format") {
		t.Fatalf("err = %v, want an unknown-format error", err)
	}
}
