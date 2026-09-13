package api

import (
	"testing"

	"github.com/Skanislav/evm-scan/internal/store"
)

// The rule this pins is that a report outranks any amount of money.
//
// The registry is open: anyone can call requestIndexing, including whoever minted the
// scam. If funding alone decided the order, then buying the most funding would buy
// the top of somebody's wallet, and the cheapest attack on this whole system would be
// to pay for it. So one report sinks a contract regardless of what was vouched for
// it — the payment still bought indexing, and the gas still subsidised the honest
// assets, it just does not come with a position.
func TestOrderAssetsReportBeatsMoney(t *testing.T) {
	in := []assetJSON{
		{Symbol: "SCAM", VouchedWei: "1000000000000000000000", Reports: 1},
		{Symbol: "SMALL", VouchedWei: "1"},
		{Symbol: "BIG", VouchedWei: "5000000000000000000"},
		{Symbol: "NONE"},
		{Symbol: "ALSOSCAM", VouchedWei: "900000000000000000000", Reports: 3},
	}
	orderAssets(in)

	got := make([]string, len(in))
	for i, a := range in {
		got[i] = a.Symbol
	}
	want := []string{"BIG", "SMALL", "NONE", "SCAM", "ALSOSCAM"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// The thousand-ETH scam sorts below a contract nobody paid a wei for. That is
	// the whole point, so it gets its own assertion rather than riding on the slice
	// comparison above.
	scam, none := -1, -1
	for i, a := range in {
		switch a.Symbol {
		case "SCAM":
			scam = i
		case "NONE":
			none = i
		}
	}
	if scam < none {
		t.Errorf("a reported asset with 1000 ETH vouched outranked an unfunded, unreported one")
	}
}

// One report and ten report the same thing: the contract is at the bottom. Counting
// beyond the first would turn ordering into a vote, and a vote on a list anyone can
// report into is a vote anyone can stuff.
func TestOrderAssetsReportCountIsNotAVote(t *testing.T) {
	in := []assetJSON{
		{Symbol: "ONE", VouchedWei: "10", Reports: 1},
		{Symbol: "MANY", VouchedWei: "20", Reports: 99},
	}
	orderAssets(in)
	if in[0].Symbol != "MANY" {
		t.Errorf("order = %s first; among reported assets the funding should still decide, not the report count", in[0].Symbol)
	}
}

// An unreadable registry must not promote everything to the top.
func TestOrderAssetsUnreadableFundingSortsAsZero(t *testing.T) {
	in := []assetJSON{
		{Symbol: "BROKEN", VouchedWei: "not a number"},
		{Symbol: "REAL", VouchedWei: "7"},
		{Symbol: "EMPTY"},
	}
	orderAssets(in)
	if in[0].Symbol != "REAL" {
		t.Errorf("order = %v; an unparseable funding figure should sort as nothing vouched", in[0].Symbol)
	}
}

// The rule from docs/SHIP.md §4, in one list: sunk rows (a report, or more
// against than for) last whatever else they have; committed first; then net
// signed demand; then vouched; then the incoming order.
func TestOrderAssetsShipRule(t *testing.T) {
	in := []assetJSON{
		{Symbol: "DUST"}, // nothing
		{Symbol: "RICH", VouchedWei: "5000000000000000000"},                                              // money only
		{Symbol: "JUNK", VouchedWei: "9000000000000000000000", Demand: demandCounts{For: 1, Against: 3}}, // against > for sinks, money or not
		{Symbol: "LIKED", Demand: demandCounts{For: 3, Against: 1}},                                      // net 2
		{Symbol: "SEEN", Committed: true},                                                                // committed, no demand
		{Symbol: "SEENLIKED", Committed: true, Demand: demandCounts{For: 1}},                             // committed, net 1
		{Symbol: "REPORTED", Committed: true, Demand: demandCounts{For: 9}, Reports: 1},                  // a report sinks even a committed, wanted row
		{Symbol: "MIXED", Demand: demandCounts{For: 2, Against: 2}, VouchedWei: "1"},                     // net 0, ties with RICH on demand, loses on money
		{Symbol: "LOVED", Demand: demandCounts{For: 5}},                                                  // net 5
	}
	orderAssets(in)
	got := make([]string, len(in))
	for i, a := range in {
		got[i] = a.Symbol
	}
	want := []string{"SEENLIKED", "SEEN", "LOVED", "LIKED", "RICH", "MIXED", "DUST", "REPORTED", "JUNK"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Sunk is a class, not a score: among sunk rows the rest of the rule still
// applies, and a row with equal for and against is not sunk.
func TestOrderAssetsSunkIsNotAScore(t *testing.T) {
	in := []assetJSON{
		{Symbol: "TIED", Demand: demandCounts{For: 4, Against: 4}},
		{Symbol: "REPORTED_RICH", Reports: 1, VouchedWei: "10"},
		{Symbol: "AGAINST_COMMITTED", Committed: true, Demand: demandCounts{Against: 1}},
	}
	orderAssets(in)
	if in[0].Symbol != "TIED" {
		t.Fatalf("a tied row sank: %s first", in[0].Symbol)
	}
	// Among the sunk, committed still leads.
	if in[1].Symbol != "AGAINST_COMMITTED" || in[2].Symbol != "REPORTED_RICH" {
		t.Fatalf("sunk order = %s, %s; committed should still lead within the sunk class", in[1].Symbol, in[2].Symbol)
	}
}

// Account rows carry the same rank and the store rows beside them move with them.
func TestOrderAccountRowsKeepsRowsAligned(t *testing.T) {
	rows := []store.AccountAsset{{EventCount: 1}, {EventCount: 2}, {EventCount: 3}}
	out := []accountAssetJSON{
		{Symbol: "A", EventCount: 1, Demand: demandCounts{Against: 1}},
		{Symbol: "B", EventCount: 2},
		{Symbol: "C", EventCount: 3, Committed: true},
	}
	orderAccountRows(rows, out)
	if out[0].Symbol != "C" || out[1].Symbol != "B" || out[2].Symbol != "A" {
		t.Fatalf("order = %s %s %s", out[0].Symbol, out[1].Symbol, out[2].Symbol)
	}
	for i := range out {
		if rows[i].EventCount != out[i].EventCount {
			t.Fatalf("row %d: store row %d beside json row %d", i, rows[i].EventCount, out[i].EventCount)
		}
	}
}
