package hintfilter

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/crypto/pbkdf2"
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

// browserPBKDF2Password is what web/hints.js was given when it produced
// testdata/browser-watch-pbkdf2.xorf, from an account's real holdings on the hosted
// mainnet index.
const browserPBKDF2Password = "correct horse battery staple"

var browserPBKDF2Tokens = []string{
	"0xdAC17F958D2ee523a2206206994597C13D831ec7", // USDT
	"0xDd44e894D8A7D765E45553347309c55a3C639A5F", // the homoglyph USDT discovery found
	"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", // USDC
	"0x5eef5946AD78E614bB3Ee7b9eD1097c517ae97f7", // WATTOIN
}

// TestGoReadsBrowserPBKDF2Watchlist is TestGoReadsBrowserWatchlist for the path a
// reader without a passkey actually takes.
//
// It pins two things the prf fixture cannot. The first is the PBKDF2 descriptor:
// SaltDesc grew a KDF name and an iteration count for it, and a descriptor that
// cannot say how the file was built is a file nobody can open — which surfaces
// months later, when the file is the only copy. The second is that the secret is
// re-derived here from what the header records rather than hardcoded, so the salt
// and the iteration count in the file have to be the ones that were used.
func TestGoReadsBrowserPBKDF2Watchlist(t *testing.T) {
	raw, err := os.ReadFile("testdata/browser-watch-pbkdf2.xorf")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !f.Blinded || f.ChainID != 1 || f.Kind != KindToken || f.Structure != StructureSortedU64 {
		t.Fatalf("metadata: blinded=%v chain=%d kind=%v structure=%v",
			f.Blinded, f.ChainID, f.Kind, f.Structure)
	}

	d, err := f.Descriptor()
	if err != nil || d == nil {
		t.Fatalf("descriptor: %+v, %v", d, err)
	}
	if d.KDF != "pbkdf2" || d.Iterations == 0 || d.KDFSalt == "" {
		t.Fatalf("descriptor does not describe how to re-derive the secret: %+v", d)
	}

	salt, err := base64.RawURLEncoding.DecodeString(d.KDFSalt)
	if err != nil {
		t.Fatalf("kdf_salt: %v", err)
	}
	secret := pbkdf2.Key([]byte(browserPBKDF2Password), salt, int(d.Iterations), 32, sha256.New)

	sub := Subkey(secret, f.ChainID, f.Kind)
	for _, a := range browserPBKDF2Tokens {
		if !f.Contains(TokenKey(sub, common.HexToAddress(a))) {
			t.Errorf("%s missing: the browser's builder and this reader disagree", a)
		}
	}

	wrong := Subkey([]byte("not the password"), f.ChainID, f.Kind)
	for _, a := range browserPBKDF2Tokens {
		if f.Contains(TokenKey(wrong, common.HexToAddress(a))) {
			t.Errorf("%s readable without the password", a)
		}
	}

	// Something that was never put in, so a filter answering yes to everything would
	// not pass by accident.
	if f.Contains(TokenKey(sub, common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"))) {
		t.Error("WETH is in a file that never held it")
	}
}
