package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/store"
)

func postVerdictWith(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/verdict", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// verdictBody signs pairs with key for account and returns the JSON body.
func verdictBody(t *testing.T, keyHex string, account common.Address, chainID uint64, deadline *big.Int, pairs []verdictPairJSON) string {
	t.Helper()
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	sv := make([]store.Verdict, len(pairs))
	for i, p := range pairs {
		sv[i] = store.Verdict{Address: common.HexToAddress(p.Address), Weight: p.Weight}
	}
	h, err := verdictDigestToSign(account, chainID, verdictDigest(sv), deadline)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27
	raw, _ := json.Marshal(verdictRequest{
		ChainID: chainID, Account: account.Hex(), Deadline: deadline.String(),
		Verdicts: pairs, Signature: hexutil.Encode(sig),
	})
	return string(raw)
}

const (
	testKeyA = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	testKeyB = "8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba"
)

func keyAddress(t *testing.T, keyHex string) common.Address {
	t.Helper()
	k, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	return crypto.PubkeyToAddress(k.PublicKey)
}

var (
	tokenX = "0x00000000000000000000000000000000000000aa"
	tokenY = "0x00000000000000000000000000000000000000bb"
)

// Every refusal below is decided before the store is touched, which is what lets
// this run with no database: a request that reached the store would panic here.
func TestVerdictRefusesBeforeTheStore(t *testing.T) {
	s := New(Deps{Chains: chainsWith(1), Log: slog.Default()})
	acct := keyAddress(t, testKeyA)
	future := big.NewInt(time.Now().Add(time.Hour).Unix())

	cases := []struct {
		name string
		body string
		code int
		want string
	}{
		{"weight zero", verdictBody(t, testKeyA, acct, 1, future, []verdictPairJSON{{tokenX, 0}}), 400, "weight must be 1 or -1"},
		{"weight two", verdictBody(t, testKeyA, acct, 1, future, []verdictPairJSON{{tokenX, 2}}), 400, "weight must be 1 or -1"},
		{"duplicate address", verdictBody(t, testKeyA, acct, 1, future, []verdictPairJSON{{tokenX, 1}, {tokenX, -1}}), 400, "appears twice"},
		{"no chain", verdictBody(t, testKeyA, acct, 0, future, []verdictPairJSON{{tokenX, 1}}), 400, "chain_id is required"},
		{"expired", verdictBody(t, testKeyA, acct, 1, big.NewInt(time.Now().Unix()-1), []verdictPairJSON{{tokenX, 1}}), 400, "expired"},
		{"now is not the future", verdictBody(t, testKeyA, acct, 1, big.NewInt(time.Now().Unix()), []verdictPairJSON{{tokenX, 1}}), 400, "expired"},
		{"bad address", `{"chain_id":1,"account":"0x1","deadline":"9999999999","verdicts":[],"signature":"0x00"}`, 400, "account must be an address"},
		{"short signature", `{"chain_id":1,"account":"` + acct.Hex() + `","deadline":"9999999999","verdicts":[],"signature":"0x00"}`, 400, "65 bytes"},
		// Signed by B, claims to be A: a stranger's signature is not a bad request,
		// it is the wrong credential.
		{"other signer", verdictBody(t, testKeyB, acct, 1, future, []verdictPairJSON{{tokenX, 1}}), 401, "not by the account"},
		// The chain is inside the message: a signature for chain 1 posted as chain
		// 8453 recovers to someone else.
		{"other chain", strings.Replace(verdictBody(t, testKeyA, acct, 1, future, []verdictPairJSON{{tokenX, 1}}), `"chain_id":1`, `"chain_id":8453`, 1), 401, "not by the account"},
		// A pair added after signing changes the digest.
		{"tampered pairs", strings.Replace(verdictBody(t, testKeyA, acct, 1, future, []verdictPairJSON{{tokenX, 1}}), `"weight":1`, `"weight":-1`, 1), 401, "not by the account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postVerdictWith(t, s, c.body)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("code %d body %s; want %d containing %q", rec.Code, rec.Body.String(), c.code, c.want)
			}
		})
	}
}

// openTestStore is the api package's copy of the store tests' gate.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("EVMSCAN_TEST_DSN")
	if dsn == "" {
		t.Skip("set EVMSCAN_TEST_DSN to a scratch Postgres to run store-backed API tests")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestVerdictRoundTrip walks SHIP.md §6's "done when" against a real Postgres: a
// signed verdict replaces a previous one, a replayed or older signature is 409,
// GET /v1/demand shows against beside voters, and the response says whether this
// deployment indexes the chain.
func TestVerdictRoundTrip(t *testing.T) {
	st := openTestStore(t)
	chainID := uint64(700_000_000 + time.Now().UnixNano()%100_000_000)
	s := New(Deps{Store: st, Chains: chainsWith(1), Log: slog.Default()})
	acct := keyAddress(t, testKeyA)
	other := keyAddress(t, testKeyB)
	d1 := big.NewInt(time.Now().Add(time.Hour).Unix())
	d2 := new(big.Int).Add(d1, big.NewInt(1))

	var out struct {
		Recorded    int  `json:"recorded"`
		Cleared     int  `json:"cleared"`
		IndexedHere bool `json:"indexed_here"`
	}
	rec := postVerdictWith(t, s, verdictBody(t, testKeyA, acct, chainID, d1, []verdictPairJSON{{tokenX, 1}, {tokenY, -1}}))
	if rec.Code != 200 {
		t.Fatalf("first verdict: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Recorded != 2 || out.Cleared != 0 || out.IndexedHere {
		t.Fatalf("first verdict = %+v; want recorded 2, cleared 0, not indexed here", out)
	}

	// The same signature again is a replay.
	rec = postVerdictWith(t, s, verdictBody(t, testKeyA, acct, chainID, d1, []verdictPairJSON{{tokenX, 1}, {tokenY, -1}}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("replay: %d %s; want 409", rec.Code, rec.Body.String())
	}

	// A later one replaces the set: Y flips to +1, X is gone.
	rec = postVerdictWith(t, s, verdictBody(t, testKeyA, acct, chainID, d2, []verdictPairJSON{{tokenY, 1}}))
	if rec.Code != 200 {
		t.Fatalf("second verdict: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Recorded != 1 || out.Cleared != 1 {
		t.Fatalf("second verdict = %+v; want recorded 1, cleared 1", out)
	}
	// And the one between them is now stale too.
	rec = postVerdictWith(t, s, verdictBody(t, testKeyA, acct, chainID, d1, []verdictPairJSON{{tokenX, 1}}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("older deadline: %d; want 409", rec.Code)
	}

	// Another account signs against Y; the listing carries both counts.
	rec = postVerdictWith(t, s, verdictBody(t, testKeyB, other, chainID, d1, []verdictPairJSON{{tokenY, -1}}))
	if rec.Code != 200 {
		t.Fatalf("other's verdict: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/demand?chain_id="+new(big.Int).SetUint64(chainID).String(), nil)
	lrec := httptest.NewRecorder()
	s.Handler().ServeHTTP(lrec, req)
	var listing struct {
		Demand []demandJSON `json:"demand"`
	}
	_ = json.Unmarshal(lrec.Body.Bytes(), &listing)
	if len(listing.Demand) != 1 || !strings.EqualFold(listing.Demand[0].Address, tokenY) ||
		listing.Demand[0].Voters != 1 || listing.Demand[0].Against != 1 {
		t.Fatalf("GET /v1/demand = %s", lrec.Body.String())
	}
}

// TestCommittedSetReadsTheFinalizedLeaf pins what `committed` means: the latest
// finalized epoch's leaf for the account, and nothing when there is no such
// epoch or no such leaf — never an error.
func TestCommittedSetReadsTheFinalizedLeaf(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	chainID := uint64(600_000_000 + time.Now().UnixNano()%100_000_000)
	s := New(Deps{Store: st, Chains: chainsWith(1), Log: slog.Default()})
	acct := keyAddress(t, testKeyA)
	x, y := common.HexToAddress(tokenX), common.HexToAddress(tokenY)

	// No epoch at all: empty, not an error.
	if got := s.committedSet(ctx, chainID, acct, nil); len(got) != 0 {
		t.Fatalf("committed with no epoch = %v", got)
	}

	id, err := st.CreateEpoch(ctx, store.Epoch{ChainID: chainID, FromBlock: 1, ToBlock: 10},
		[]store.EpochLeaf{{Index: 0, Account: acct, Assets: []common.Address{x, y}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Built and published are not finalized.
	if got := s.committedSet(ctx, chainID, acct, nil); len(got) != 0 {
		t.Fatalf("committed with an unfinalized epoch = %v", got)
	}
	if err := st.MarkPublished(ctx, id, 7, common.Hash{1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEpochStatus(ctx, id, store.EpochFinalized); err != nil {
		t.Fatal(err)
	}
	got := s.committedSet(ctx, chainID, acct, nil)
	if len(got) != 2 || !got[x] || !got[y] {
		t.Fatalf("committed = %v; want exactly {%s, %s}", got, tokenX, tokenY)
	}
	// Another account has no leaf in it.
	if got := s.committedSet(ctx, chainID, keyAddress(t, testKeyB), nil); len(got) != 0 {
		t.Fatalf("committed for a stranger = %v", got)
	}
}
