package api

import "testing"

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
