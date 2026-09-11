package hintfilter

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// browserWatchSecret and browserWatchTokens are what web/index.html's buildWatchlist
// was given when it produced testdata/browser-watch.xorf.
var (
	browserWatchSecret = []byte("a passkey prf output would go here")
	browserWatchTokens = []string{
		"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		"0xdAC17F958D2ee523a2206206994597C13D831ec7",
		"0x6B175474E89094C44Da98b954EedeAC495271d0F",
	}
)

// TestGoReadsBrowserWatchlist runs the format the other way round.
//
// Everywhere else Go writes and the browser reads, and testdata/vectors.json pins
// that direction. A blinded watchlist is the one artifact built only in the browser,
// so without this fixture the JavaScript builder could drift from the Go reader and
// nothing would notice until someone's private watchlist would not open — at which
// point the file is the only copy and the drift is not recoverable.
func TestGoReadsBrowserWatchlist(t *testing.T) {
	raw, err := os.ReadFile("testdata/browser-watch.xorf")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if !f.Blinded {
		t.Error("blinded flag lost: the file would read as public and answer no to everything")
	}
	if f.ChainID != 1 || f.Kind != KindToken || f.Structure != StructureSortedU64 {
		t.Errorf("metadata: chain=%d kind=%v structure=%v", f.ChainID, f.Kind, f.Structure)
	}
	if f.Count() != uint64(len(browserWatchTokens)) {
		t.Fatalf("Count() = %d, want %d", f.Count(), len(browserWatchTokens))
	}

	d, err := f.Descriptor()
	if err != nil || d == nil || d.KDF != "webauthn-prf" {
		t.Fatalf("descriptor did not survive: %+v, %v", d, err)
	}

	sub := Subkey(browserWatchSecret, 1, KindToken)
	for _, a := range browserWatchTokens {
		if !f.Contains(TokenKey(sub, common.HexToAddress(a))) {
			t.Errorf("%s missing: the browser's builder and this reader disagree", a)
		}
	}

	// And the property the whole blinded mode rests on: the same file, the same
	// public token addresses anyone can guess, the wrong secret, and nothing comes
	// back.
	wrong := Subkey([]byte("wrong secret"), 1, KindToken)
	for _, a := range browserWatchTokens {
		if f.Contains(TokenKey(wrong, common.HexToAddress(a))) {
			t.Errorf("%s readable without the secret", a)
		}
	}
}
