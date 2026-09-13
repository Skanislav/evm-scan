package store_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// TestVerdictsReplaceAndCountBothWays runs the verdict SQL against a real
// Postgres: a signed split replaces the voter's previous rows rather than adding
// to them, a stale deadline is refused, against sinks a contract out of promotion,
// and a plain /v1/demand vote still counts as +1 without flipping a signed -1.
func TestVerdictsReplaceAndCountBothWays(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	chain := uint64(800_000_000 + time.Now().UnixNano()%100_000_000)

	a := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	b := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	c := common.HexToAddress("0x00000000000000000000000000000000000000cc")
	acct1 := common.HexToAddress("0x0000000000000000000000000000000000000001")
	acct2 := common.HexToAddress("0x0000000000000000000000000000000000000002")

	// A first verdict: a and b recognized, c not.
	rec, cl, err := st.ReplaceVerdicts(ctx, chain, acct1, big.NewInt(100), []store.Verdict{{Address: a, Weight: 1}, {Address: b, Weight: 1}, {Address: c, Weight: -1}})
	if err != nil || rec != 3 || cl != 0 {
		t.Fatalf("first verdict: recorded %d cleared %d err %v", rec, cl, err)
	}
	got, err := st.DemandFor(ctx, chain, []common.Address{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if got[a] != (store.DemandTotals{For: 1}) || got[b] != (store.DemandTotals{For: 1}) || got[c] != (store.DemandTotals{Against: 1}) {
		t.Fatalf("DemandFor after first verdict = %v", got)
	}

	// The same deadline again is a replay; an earlier one too.
	if _, _, err := st.ReplaceVerdicts(ctx, chain, acct1, big.NewInt(100), []store.Verdict{{Address: a, Weight: 1}}); !errors.Is(err, store.ErrStaleVerdict) {
		t.Fatalf("replayed deadline: err %v, want ErrStaleVerdict", err)
	}
	if _, _, err := st.ReplaceVerdicts(ctx, chain, acct1, big.NewInt(99), []store.Verdict{{Address: a, Weight: 1}}); !errors.Is(err, store.ErrStaleVerdict) {
		t.Fatalf("earlier deadline: err %v, want ErrStaleVerdict", err)
	}
	// And a refused one changed nothing.
	got, _ = st.DemandFor(ctx, chain, []common.Address{a, b, c})
	if got[a] != (store.DemandTotals{For: 1}) || got[b] != (store.DemandTotals{For: 1}) || got[c] != (store.DemandTotals{Against: 1}) {
		t.Fatalf("DemandFor after refused verdicts = %v", got)
	}

	// A later signature replaces the whole set: c flips to recognized, b drops out.
	rec, cl, err = st.ReplaceVerdicts(ctx, chain, acct1, big.NewInt(101), []store.Verdict{{Address: a, Weight: 1}, {Address: c, Weight: 1}})
	if err != nil || rec != 2 || cl != 1 {
		t.Fatalf("second verdict: recorded %d cleared %d err %v", rec, cl, err)
	}
	got, _ = st.DemandFor(ctx, chain, []common.Address{a, b, c})
	if got[a] != (store.DemandTotals{For: 1}) || got[b] != (store.DemandTotals{}) || got[c] != (store.DemandTotals{For: 1}) {
		t.Fatalf("DemandFor after second verdict = %v", got)
	}

	// A second account marks a as junk: net zero, so it no longer clears
	// min_voters 1 as a demand-only promotion, while c (net 1) still does.
	if _, _, err := st.ReplaceVerdicts(ctx, chain, acct2, big.NewInt(50), []store.Verdict{{Address: a, Weight: -1}}); err != nil {
		t.Fatal(err)
	}
	un, err := st.DemandedUnseen(ctx, chain, 1, 10)
	if err != nil || len(un) != 1 || un[0].Address != c {
		t.Fatalf("DemandedUnseen = %+v, %v; want only c", un, err)
	}
	if err := st.UpsertCandidates(ctx, chain, []store.Candidate{
		{ChainID: chain, Address: a, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 5, EventCount: 10, BlocksSeen: 5},
		{ChainID: chain, Address: c, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 5, EventCount: 10, BlocksSeen: 5},
	}); err != nil {
		t.Fatal(err)
	}
	cs, err := st.PromotableCandidates(ctx, chain, store.PromotionRule{MinVoters: 1}, 10)
	if err != nil || len(cs) != 1 || cs[0].Address != c {
		t.Fatalf("PromotableCandidates = %+v, %v; want only c", cs, err)
	}

	// The listing carries both counts, net first.
	dl, err := st.ListDemand(ctx, chain, 10)
	if err != nil || len(dl) != 2 {
		t.Fatalf("ListDemand = %+v, %v", dl, err)
	}
	if dl[0].Address != c || dl[0].Voters != 1 || dl[0].Against != 0 ||
		dl[1].Address != a || dl[1].Voters != 1 || dl[1].Against != 1 {
		t.Fatalf("ListDemand = %+v", dl)
	}

	// A plain vote is +1 on a fresh pair, and does not flip a signed -1: the
	// unsigned write is the weaker one.
	if n, err := st.RecordDemand(ctx, chain, acct2, []common.Address{a, b}); err != nil || n != 1 {
		t.Fatalf("RecordDemand: added %d, err %v; want 1", n, err)
	}
	got, _ = st.DemandFor(ctx, chain, []common.Address{a, b})
	if got[a] != (store.DemandTotals{For: 1, Against: 1}) || got[b] != (store.DemandTotals{For: 1}) {
		t.Fatalf("DemandFor after plain vote = %v", got)
	}

	// Retracting everything is a valid verdict.
	rec, cl, err = st.ReplaceVerdicts(ctx, chain, acct2, big.NewInt(51), nil)
	if err != nil || rec != 0 || cl != 2 {
		t.Fatalf("retract: recorded %d cleared %d err %v", rec, cl, err)
	}
	// A bad weight never reaches the database.
	if _, _, err := st.ReplaceVerdicts(ctx, chain, acct2, big.NewInt(52), []store.Verdict{{Address: a, Weight: 0}}); err == nil {
		t.Fatal("weight 0 was accepted")
	}
}
