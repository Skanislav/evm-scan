package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/hintfilter"
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
