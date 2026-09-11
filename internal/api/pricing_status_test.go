package api

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/price"
)

func TestPricingStatusExplainsItsAbsence(t *testing.T) {
	// Sepolia has no built-in sources: nothing to read unless the operator names one.
	sep := pricingStatusFor(11155111, nil, "no liquidity worth quoting")
	if sep.Enabled {
		t.Fatal("no pricer, yet enabled")
	}
	if !strings.Contains(sep.Reason, "no built-in") || !strings.Contains(sep.Reason, "11155111") {
		t.Fatalf("reason should say the chain has no built-in sources: %q", sep.Reason)
	}
	if sep.Note != "no liquidity worth quoting" {
		t.Fatalf("operator note dropped: %q", sep.Note)
	}

	// Mainnet has built-ins, so a missing pricer there means config turned it off.
	main := pricingStatusFor(1, nil, "")
	if !strings.Contains(main.Reason, "switched off") {
		t.Fatalf("reason should point at config: %q", main.Reason)
	}
	if main.Note != "" {
		t.Fatalf("no note given, yet one reported: %q", main.Note)
	}

	// With sources the block describes them and carries neither reason nor note.
	p := price.New(nil, price.Config{Sources: price.Sources{
		FeedRegistry: common.HexToAddress("0x47Fb2585D2C56Fe188D0E6ec628a38b74fCeeeDf"),
	}}, nil)
	on := pricingStatusFor(1, p, "stale note")
	if !on.Enabled || !on.FeedRegistry || !on.NativeFeed {
		t.Fatalf("sources not described: %+v", on)
	}
	if on.Reason != "" || on.Note != "" {
		t.Fatalf("an enabled chain needs no excuse: %+v", on)
	}
}
