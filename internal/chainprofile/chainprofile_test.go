package chainprofile

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/price"
)

// TestConfirmationsAreWallClock is the test that actually matters here. The whole
// reason this table exists is that a confirmation depth copied between chains means
// a different amount of time on each, so assert the thing we meant rather than the
// numbers we typed.
func TestConfirmationsAreWallClock(t *testing.T) {
	for id, p := range known {
		if p.Confirmations == 0 {
			continue // dev chains mine on demand; there is nothing to wait for
		}
		d := time.Duration(p.Confirmations) * p.BlockTime
		if d < 30*time.Second || d > 4*time.Minute {
			t.Errorf("chain %d (%s): %d confirmations at %s per block is %s, "+
				"outside the 30s–4m the table is supposed to hold to",
				id, p.Name, p.Confirmations, p.BlockTime, d)
		}
	}
}

func TestEveryProfileIsUsable(t *testing.T) {
	for id, p := range known {
		if p.Name == "" {
			t.Errorf("chain %d has no name", id)
		}
		if p.NativeSymbol == "" || p.NativeDecimals <= 0 {
			t.Errorf("chain %d (%s) has no native asset", id, p.Name)
		}
		if p.BlockTime <= 0 {
			t.Errorf("chain %d (%s) has no block time", id, p.Name)
		}
		// A zero window would mean SweepLogs silently substituting its own 10,000,
		// which is exactly the "inherited the wrong chain's numbers" failure.
		if p.BackfillWindow == 0 || p.TailWindow == 0 {
			t.Errorf("chain %d (%s) has a zero log window", id, p.Name)
		}
		if p.PollInterval <= 0 || p.BackfillInterval <= 0 {
			t.Errorf("chain %d (%s) has a zero interval", id, p.Name)
		}
	}
}

// TestWrappedNativeAgreesWithPricing keeps the two tables from drifting apart.
// price.Defaults already names each chain's wrapped native as a quote token, and
// two different answers to "what is WETH on Base" would be a genuinely confusing
// bug to chase.
func TestWrappedNativeAgreesWithPricing(t *testing.T) {
	for id, p := range known {
		if p.WrappedNative == (common.Address{}) {
			continue
		}
		src, ok := price.Defaults(id)
		if !ok {
			continue
		}
		for _, q := range src.QuoteTokens {
			if !q.WrappedNative {
				continue
			}
			if q.Address != p.WrappedNative {
				t.Errorf("chain %d (%s): wrapped native is %s here and %s in price.Defaults",
					id, p.Name, p.WrappedNative, q.Address)
			}
		}
	}
}

func TestForFallsBackWithoutLying(t *testing.T) {
	// A chain we know keeps its own numbers.
	if got := For(8453, "whatever"); got.Name != "base" || got.Confirmations != 30 {
		t.Errorf("For(8453) = %+v, want the base profile", got)
	}
	// One we do not gets the timid defaults and the caller's name.
	got := For(999999, "arc-testnet")
	if got.Name != "arc-testnet" {
		t.Errorf("name = %q, want the supplied one", got.Name)
	}
	if got.BackfillWindow != Generic().BackfillWindow {
		t.Errorf("unknown chain did not get the generic window")
	}
	if _, ok := Lookup(999999); ok {
		t.Error("Lookup claims to know a chain it does not; the API would call the guesses facts")
	}
}
