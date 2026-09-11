package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/store"
)

var (
	usdc = common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	junk = common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
)

// staticTokenFilter builds a one-token filter the way LoadTokenLists would.
func staticTokenFilter(t *testing.T, chainID uint64, tokens ...common.Address) *hintfilter.Cache {
	t.Helper()
	sub := hintfilter.Subkey(hintfilter.PublicSecret, chainID, hintfilter.KindToken)
	keys := make([]uint64, len(tokens))
	for i, a := range tokens {
		keys[i] = hintfilter.TokenKey(sub, a)
	}
	f, err := hintfilter.Build(keys, hintfilter.Meta{ChainID: chainID, Kind: hintfilter.KindToken, EpochID: -1})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return hintfilter.NewStatic(f, hintfilter.BuildManifest(f, enc, "test", ""), enc)
}

// chainsWith is the smallest chain set the router needs: ids only, no nodes. The
// filter endpoints never touch a chain's source, so nothing here has to dial.
func chainsWith(ids ...uint64) *chainset.Set {
	set := chainset.New()
	for _, id := range ids {
		_ = set.Add(&chainset.Entry{ID: id, Name: "test"})
	}
	return set
}

func hintServer(t *testing.T) *Server {
	t.Helper()
	return New(Deps{
		Chains:       chainsWith(1),
		TokenFilters: map[uint64]*hintfilter.Cache{1: staticTokenFilter(t, 1, usdc)},
		Log:          slog.Default(),
	})
}

func TestServeHintFilter(t *testing.T) {
	s := hintServer(t)

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hints/tokens-1.xorf", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	// The bytes served must be a filter that still answers correctly. Serving
	// something that decodes but has lost its contents is the failure a status
	// check alone would miss.
	f, err := hintfilter.Decode(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("served bytes do not decode: %v", err)
	}
	sub := hintfilter.Subkey(hintfilter.PublicSecret, 1, hintfilter.KindToken)
	if !f.Contains(hintfilter.TokenKey(sub, usdc)) {
		t.Error("the listed token is missing from the served filter")
	}

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag: a deterministic artifact should be revalidatable")
	}

	// A deterministic format means an unchanged filter costs a repeat visitor no
	// bytes at all.
	req := httptest.NewRequest(http.MethodGet, "/v1/hints/tokens-1.xorf", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("revalidation returned %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body", rec2.Body.Len())
	}
}

// TestHintNameIsNotAPath is the one that matters for safety: {name} is
// user-controlled, and a handler that turned it into a filesystem path would be the
// textbook traversal.
func TestHintNameIsNotAPath(t *testing.T) {
	s := hintServer(t)
	for _, name := range []string{
		"nope", "..%2f..%2fetc%2fpasswd", "../../../../etc/passwd", "tokens-999",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hints/"+name+".xorf", nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%q was served with status 200", name)
		}
	}
}

func TestListHints(t *testing.T) {
	s := hintServer(t)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hints", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var body struct {
		Filters []hintFilterJSON `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Filters) != 1 {
		t.Fatalf("got %d filters, want 1", len(body.Filters))
	}
	f := body.Filters[0]
	if f.Name != "tokens-1" || f.Count != 1 || f.ChainID != 1 || f.KeyKind != "token" {
		t.Errorf("unexpected listing: %+v", f)
	}
	// The derivation is published so a client never has to transcribe the
	// constants. A client that copies them by hand computes keys nobody else does.
	if f.Derivation.Domain != "evmscan/xorf/v1" || f.Derivation.Subkey == "" {
		t.Errorf("derivation not published: %+v", f.Derivation)
	}
	if f.Keccak == "" {
		t.Error("no digest published: the artifact cannot be checked without one")
	}
}

func TestKnownToken(t *testing.T) {
	s := hintServer(t)
	ctx := t.Context()

	if known, have := s.knownToken(ctx, 1, usdc); !known || !have {
		t.Errorf("listed token: known=%v haveList=%v, want true true", known, have)
	}
	if known, have := s.knownToken(ctx, 1, junk); known || !have {
		t.Errorf("unlisted token: known=%v haveList=%v, want false true", known, have)
	}

	// A chain with no list configured must report haveList=false, not known=false.
	// Collapsing the two would mark every token junk on a deployment that simply
	// never configured a list.
	if known, have := s.knownToken(ctx, 999, usdc); known || have {
		t.Errorf("chain with no list: known=%v haveList=%v, want false false", known, have)
	}
}

func TestNoTokenFilterServesNoHints(t *testing.T) {
	s := New(Deps{Chains: chainsWith(1), Log: slog.Default()})
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hints", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Filters []hintFilterJSON `json:"filters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Filters) != 0 {
		t.Errorf("a deployment with no lists served %d filters", len(body.Filters))
	}
}

// TestEpochManifestNamesTheFilterDigest pins the one JSON key that the verifier
// reads back out (cmd/evmscan-verify/filter.go, manifestDigest).
//
// This is a contract between two programs that never call each other: the daemon
// writes index_filter.keccak256, evmscan-verify parses it, and a rename on either
// side turns "the file does not match the chain" into the error a reader sees for a
// tampered download. There is nothing in the type system holding them together, so
// there is a test.
func TestEpochManifestNamesTheFilterDigest(t *testing.T) {
	s := hintServer(t)

	// The handler needs a store; without one it must say so rather than panic.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/epochs/1/manifest", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("store-less manifest returned %d, want 503", rec.Code)
	}

	// The shape the verifier expects, asserted against the literal keys it parses.
	digest := common.HexToHash("0xabc123")
	body := manifestBody(store.Epoch{
		ID: 7, ChainID: 1, FromBlock: 10, ToBlock: 99,
		MerkleRoot: common.HexToHash("0xdead"), FilterKeccak: digest,
	})
	var parsed struct {
		IndexFilter struct {
			Keccak256 string `json:"keccak256"`
			ToBlock   uint64 `json:"to_block"`
		} `json:"index_filter"`
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.IndexFilter.Keccak256 != digest.Hex() {
		t.Errorf("index_filter.keccak256 = %q, want %q", parsed.IndexFilter.Keccak256, digest.Hex())
	}
	if parsed.IndexFilter.ToBlock != 99 {
		t.Errorf("index_filter.to_block = %d, want 99", parsed.IndexFilter.ToBlock)
	}

	// An epoch built before migration 0008 names no filter, and must not claim an
	// all-zero digest: a reader would check a real file against it and be told the
	// file is wrong.
	bare, err := json.Marshal(manifestBody(store.Epoch{ID: 1, ChainID: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "index_filter") {
		t.Errorf("an epoch with no filter still advertised one: %s", bare)
	}
}
