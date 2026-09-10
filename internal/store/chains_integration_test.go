package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// These run against a real Postgres, because the thing worth testing about
// hand-written SQL is whether Postgres accepts it. Skipped without one:
//
//	EVMSCAN_TEST_DSN="postgres://evmscan:evmscan@127.0.0.1:55432/evmscan?sslmode=disable" \
//	  go test ./internal/store/ -v
func open(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("EVMSCAN_TEST_DSN")
	if dsn == "" {
		t.Skip("set EVMSCAN_TEST_DSN to a scratch Postgres to run store tests")
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

func TestChainProfileRoundTrip(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	wrapped := common.HexToAddress("0x4200000000000000000000000000000000000006")
	resolved := time.Now().UTC().Truncate(time.Second)
	in := store.ChainProfile{
		ChainID: 8453, Name: "base", Enabled: true,
		Source: store.ChainSourceAPI, Trust: store.TrustUnverified,
		NodeURL:      "https://base.example/v1/SECRET",
		NativeSymbol: "ETH", NativeDecimals: 18, WrappedNative: &wrapped,
		Tuning:      json.RawMessage(`{"confirmations":30,"tail_window":500}`),
		ENSName:     "base.on.eth",
		ENSResolved: &resolved,
	}
	if err := st.UpsertChainProfile(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := st.GetChainProfile(ctx, 8453)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "base" || got.Source != store.ChainSourceAPI || got.Trust != store.TrustUnverified {
		t.Errorf("got %+v", got)
	}
	if got.NodeURL != in.NodeURL {
		t.Errorf("node url = %q", got.NodeURL)
	}
	if got.WrappedNative == nil || *got.WrappedNative != wrapped {
		t.Errorf("wrapped native = %v", got.WrappedNative)
	}
	if got.ENSName != "base.on.eth" || got.ENSResolved == nil {
		t.Errorf("ens = %q %v", got.ENSName, got.ENSResolved)
	}
	var tuning map[string]any
	if err := json.Unmarshal(got.Tuning, &tuning); err != nil {
		t.Fatalf("tuning did not round-trip: %v", err)
	}
	if tuning["confirmations"] != float64(30) {
		t.Errorf("tuning = %v", tuning)
	}
}

// TestUpsertDoesNotUndoATrustPromotion is the one with teeth. An operator raising
// a chain to verified is a deliberate decision to stake the publisher's bond on
// someone else's node; a restart re-reading the YAML must neither undo it nor
// silently re-apply it.
func TestUpsertDoesNotUndoATrustPromotion(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	base := store.ChainProfile{ChainID: 10, Name: "optimism", Enabled: true,
		Source: store.ChainSourceConfig, NodeURL: "http://127.0.0.1:8545"}
	if err := st.UpsertChainProfile(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChainTrust(ctx, 10, store.TrustVerified, "operator@laptop"); err != nil {
		t.Fatal(err)
	}

	// A restart: same config, written again.
	if err := st.UpsertChainProfile(ctx, base); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetChainProfile(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trust != store.TrustVerified {
		t.Errorf("a restart demoted a verified chain to %q", got.Trust)
	}
	if got.TrustSetBy != "operator@laptop" || got.TrustSetAt == nil {
		t.Errorf("the promotion left no audit trail: by=%q at=%v", got.TrustSetBy, got.TrustSetAt)
	}

	if err := st.SetChainTrust(ctx, 10, "nonsense", ""); err == nil {
		t.Error("SetChainTrust accepted a level that is not one")
	}
	if err := st.SetChainTrust(ctx, 999999, store.TrustVerified, ""); err == nil {
		t.Error("SetChainTrust invented a chain")
	}
}

func TestChainEnabledAndErrors(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	if err := st.UpsertChainProfile(ctx, store.ChainProfile{
		ChainID: 42161, Name: "arbitrum", Enabled: true, Source: store.ChainSourceAPI,
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.SetChainError(ctx, 42161, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetChainProfile(ctx, 42161)
	if got.LastError == "" || got.LastErrorAt == nil {
		t.Error("the error was not recorded")
	}
	if err := st.SetChainError(ctx, 42161, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetChainProfile(ctx, 42161); got.LastError != "" {
		t.Errorf("the error was not cleared: %q", got.LastError)
	}

	if err := st.SetChainEnabled(ctx, 42161, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := st.ListChainProfiles(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range enabled {
		if p.ChainID == 42161 {
			t.Error("a disabled chain came back from the enabled-only list")
		}
	}
	// Disabling keeps the row: the index it built is still worth something.
	all, err := st.ListChainProfiles(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range all {
		if p.ChainID == 42161 {
			found = true
		}
	}
	if !found {
		t.Error("disabling a chain deleted it")
	}
}
