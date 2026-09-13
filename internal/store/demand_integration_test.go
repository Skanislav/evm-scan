package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// TestDemandCountsVotersAndOrdersPromotion runs the demand SQL against a real
// Postgres: one account counts once however often it votes, totals add the
// on-chain aggregate, demand orders and can alone qualify a candidate, a voted
// contract discovery never saw is reachable, and a spam verdict still wins.
func TestDemandCountsVotersAndOrdersPromotion(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	chain := uint64(900_000_000 + time.Now().UnixNano()%100_000_000)

	a := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	b := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	c := common.HexToAddress("0x00000000000000000000000000000000000000cc")
	acct1 := common.HexToAddress("0x0000000000000000000000000000000000000001")
	acct2 := common.HexToAddress("0x0000000000000000000000000000000000000002")

	if err := st.UpsertCandidates(ctx, chain, []store.Candidate{
		{ChainID: chain, Address: a, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 5, EventCount: 10, BlocksSeen: 5},
		{ChainID: chain, Address: b, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 50, EventCount: 500, BlocksSeen: 50},
	}); err != nil {
		t.Fatalf("upsert candidates: %v", err)
	}

	if n, err := st.RecordDemand(ctx, chain, acct1, []common.Address{a, c, a}); err != nil || n != 2 {
		t.Fatalf("first vote: added %d, err %v; want 2", n, err)
	}
	if n, err := st.RecordDemand(ctx, chain, acct1, []common.Address{a, c}); err != nil || n != 0 {
		t.Fatalf("repeat vote: added %d, err %v; want 0", n, err)
	}
	if n, err := st.RecordDemand(ctx, chain, acct2, []common.Address{a}); err != nil || n != 1 {
		t.Fatalf("second voter: added %d, err %v; want 1", n, err)
	}

	got, err := st.DemandFor(ctx, chain, []common.Address{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if got[a].For != 2 || got[c].For != 1 || got[b].For != 0 {
		t.Fatalf("DemandFor = %v", got)
	}

	addrs := func(cs []store.Candidate) []common.Address {
		out := make([]common.Address, len(cs))
		for i, x := range cs {
			out[i] = x.Address
		}
		return out
	}
	// Demand alone, activity off: only a clears two voters.
	cs, err := st.PromotableCandidates(ctx, chain, store.PromotionRule{MinVoters: 2}, 10)
	if err != nil || len(cs) != 1 || cs[0].Address != a || cs[0].Voters != 2 {
		t.Fatalf("demand-only promotable = %v, %v", addrs(cs), err)
	}
	// Activity alone: only b is busy enough.
	cs, err = st.PromotableCandidates(ctx, chain, store.PromotionRule{Activity: true, MinEvents: 100, MinBlocks: 10}, 10)
	if err != nil || len(cs) != 1 || cs[0].Address != b {
		t.Fatalf("activity-only promotable = %v, %v", addrs(cs), err)
	}
	// Both: a first because it was asked for, then b because it is busy.
	cs, err = st.PromotableCandidates(ctx, chain, store.PromotionRule{Activity: true, MinEvents: 100, MinBlocks: 10, MinVoters: 1}, 10)
	if err != nil || len(cs) != 2 || cs[0].Address != a || cs[1].Address != b {
		t.Fatalf("combined promotable = %v, %v", addrs(cs), err)
	}
	// The listing puts the wanted one first too.
	ls, err := st.ListCandidates(ctx, chain, store.CandidateFilter{Limit: 10})
	if err != nil || len(ls) != 2 || ls[0].Address != a {
		t.Fatalf("ListCandidates = %v, %v", addrs(ls), err)
	}

	// c was voted for and never seen by discovery.
	un, err := st.DemandedUnseen(ctx, chain, 1, 10)
	if err != nil || len(un) != 1 || un[0].Address != c || un[0].Voters != 1 || un[0].Candidate || un[0].Indexed {
		t.Fatalf("DemandedUnseen = %+v, %v", un, err)
	}

	// The on-chain aggregate adds to the total and reorders the listing, and a
	// sync replaces the whole table: a row the registry no longer lists is gone.
	if err := st.SetOnchainDemand(ctx, chain, c, 9); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceOnchainDemand(ctx, []store.DemandRow{{ChainID: chain, Address: b, OnchainVoters: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceOnchainDemand(ctx, []store.DemandRow{{ChainID: chain, Address: b, OnchainVoters: 4}}); err != nil {
		t.Fatal(err)
	}
	dl, err := st.ListDemand(ctx, chain, 10)
	if err != nil || len(dl) != 3 || dl[0].Address != b || dl[0].Voters != 4 || dl[0].OnchainVoters != 4 ||
		dl[1].Address != a || dl[1].Voters != 2 || dl[2].Address != c {
		t.Fatalf("ListDemand = %+v, %v", dl, err)
	}
	if !dl[0].Candidate || dl[0].EventCount != 500 || dl[2].Candidate {
		t.Fatalf("ListDemand status = %+v", dl)
	}

	// A spam verdict outranks any number of votes.
	if err := st.MarkCandidateSpam(ctx, chain, a, "test"); err != nil {
		t.Fatal(err)
	}
	cs, err = st.PromotableCandidates(ctx, chain, store.PromotionRule{MinVoters: 1}, 10)
	if err != nil || len(cs) != 1 || cs[0].Address != b {
		t.Fatalf("promotable after spam = %v, %v", addrs(cs), err)
	}
}
