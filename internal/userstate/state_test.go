package userstate

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var updateFixture = flag.Bool("update-state", false, "regenerate state fixtures")

func fixture(t *testing.T) Snapshot {
	t.Helper()
	key, err := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Version: 1, Account: crypto.PubkeyToAddress(key.PublicKey), Revision: "1", Deadline: "2000000000", Entries: []Entry{{Kind: "verdict", ChainID: "1", Address: common.HexToAddress("0x11"), Weight: 1}, {Kind: "verdict", ChainID: "8453", Address: common.HexToAddress("0x22"), Weight: -1}, {Kind: "asset", ChainID: "8453", Address: common.HexToAddress("0x22")}}}
	tr, err := Build(s.Entries)
	if err != nil {
		t.Fatal(err)
	}
	s.StateRoot = tr.Root
	id, err := s.ID()
	if err != nil {
		t.Fatal(err)
	}
	s.Signature, err = crypto.Sign(id[:], key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestStateProofsAndFixture(t *testing.T) {
	s := fixture(t)
	id, tr, err := s.Validate()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Entries {
		p, _ := e.Pair()
		proof := tr.Proof(p.Key)
		if !VerifyProof(tr.Root, proof) {
			t.Fatal("membership proof")
		}
		proof.Value = []byte{17}
		if VerifyProof(tr.Root, proof) {
			t.Fatal("accepted altered value")
		}
	}
	absence := tr.Proof(common.Hash{})
	if len(absence.Value) != 0 || !VerifyProof(tr.Root, absence) {
		t.Fatal("absence proof")
	}
	absence.Siblings = absence.Siblings[:255]
	if VerifyProof(tr.Root, absence) {
		t.Fatal("accepted truncated proof")
	}
	reversed := []Entry{s.Entries[2], s.Entries[1], s.Entries[0]}
	other, _ := Build(reversed)
	if tr.Root != other.Root {
		t.Fatal("insertion order changed root")
	}
	empty, _ := Build(nil)
	if empty.Root != Empty[0] {
		t.Fatal("empty root")
	}
	if _, err := Build(append(reversed, s.Entries[0])); err == nil {
		t.Fatal("duplicate allowed")
	}
	accounts := []AccountRevision{{s.Account, id}}
	agg, _ := Aggregate(accounts)
	fixture := struct {
		Snapshot   Snapshot    `json:"snapshot"`
		ID         common.Hash `json:"id"`
		Empty      common.Hash `json:"empty_root"`
		Checkpoint Checkpoint  `json:"checkpoint"`
		Proof      Proof       `json:"proof"`
		Absent     Proof       `json:"absent"`
	}{s, id, Empty[0], Checkpoint{1, agg.Root, accounts, []Snapshot{s}}, agg.Proof(AccountKey(s.Account)), agg.Proof(AccountKey(common.Address{}))}
	raw, _ := json.MarshalIndent(fixture, "", "  ")
	raw = append(raw, '\n')
	path := filepath.Join("testdata", "state.json")
	if *updateFixture {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(raw) {
		t.Fatal("state fixture changed; explicitly regenerate and verify JavaScript")
	}
}
func TestStateRejectsTampering(t *testing.T) {
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Version = 2 }, func(s *Snapshot) { s.Revision = "01" }, func(s *Snapshot) { s.Deadline = "-1" }, func(s *Snapshot) { s.Entries[1].Weight = 1 }, func(s *Snapshot) { s.Account = common.Address{} }, func(s *Snapshot) { s.Previous = common.HexToHash("0x11") }, func(s *Snapshot) { s.Signature[64] = 5 }} {
		s := fixture(t)
		change(&s)
		if _, _, err := s.Validate(); err == nil {
			t.Fatal("accepted invalid snapshot")
		}
	}
}
func TestCheckpointRequiresCompleteExport(t *testing.T) {
	s := fixture(t)
	id, _, _ := s.Validate()
	accounts := []AccountRevision{{s.Account, id}}
	tr, _ := Aggregate(accounts)
	c := Checkpoint{Version: 1, Root: tr.Root, Accounts: accounts}
	if c.Validate() == nil {
		t.Fatal("accepted missing snapshot")
	}
	c.Snapshots = []Snapshot{s}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
