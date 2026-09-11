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

// browserSlotTokens is what web/hints.js had on screen when it produced
// testdata/browser-slot-interop.xorf: a real account's mainnet holdings, read off
// the hosted index.
var browserSlotTokens = []string{
	"0x40D16FC0246aD3160Ccc09B8D0D3A2cD28aE6C2f", "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
	"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "0xdAC17F958D2ee523a2206206994597C13D831ec7",
	"0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599", "0x111111111117dC0aa78b770fA6A738034120C302",
	"0x58b6A8A3302369DAEc383334672404Ee733aB239", "0x06450dEe7FD2Fb8E39061434BAbCFC05599a6Fb8",
	"0x5eef5946AD78E614bB3Ee7b9eD1097c517ae97f7", "0x6B175474E89094C44Da98b954EedeAC495271d0F",
	"0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984", "0x7f39C581F595B53c5cb19bD0b3f8dA6c935E2Ca0",
	"0xdC035D45d973E3EC169d2276DDab16f1e407384F", "0x514910771AF9Ca656af840dff83E8264EcF986CA",
	"0xD533a949740bb3306d119CC777fa900bA034cd52", "0x6c3ea9036406852006290770BEdFcAbA0e23A0e8",
	"0x56072C95FAA701256059aa122697B133aDEd9279", "0x6982508145454Ce325dDbE47a25d4ec3d2311933",
	"0x6a77E39240dA69Ea788e4cF93663D2c41EA4b12b", "0x7Fc66500c84A76Ad7e9c93437bFc5Ac33E2DDaE9",
	"0x1776e1F26f98b1A5dF9cD347953a26dd3Cb46671", "0xe343167631d89B6Ffc58B88d6b7fB0228795491D",
	"0xAcE8E719899F6E91831B18AE746C9A965c2119F1",
}

// TestGoReadsBrowserSlot runs the one-slot hint the other way round.
//
// The vectors have Go writing and the browser reading. This is the reverse, and it
// matters more for this structure than for the others, because the browser is the
// only thing that builds a per-account hint — the daemon never sees the account.
// Three implementations have to agree on the probe (Go, the reader in index.html,
// the builder in hints.js) and only a fixture built by the third one pins it.
func TestGoReadsBrowserSlot(t *testing.T) {
	raw, err := os.ReadFile("testdata/browser-slot-interop.xorf")
	if err != nil {
		t.Fatal(err)
	}
	f, err := Decode(raw)
	if err != nil {
		t.Fatalf("Go cannot read what the browser built: %v", err)
	}
	if f.Structure != StructureBloom || f.Kind != KindInterop {
		t.Fatalf("structure=%v kind=%v", f.Structure, f.Kind)
	}
	// chainId 0 because the chain rides in every preimage instead.
	if f.ChainID != 0 {
		t.Errorf("chain id %d: an interop filter binds no single chain", f.ChainID)
	}
	if m, _ := f.BloomParams(); m != 256 {
		t.Errorf("m = %d, want the 256 bits that fit a storage slot", m)
	}
	if _, err := f.Slot(); err != nil {
		t.Errorf("Slot: %v", err)
	}

	sub := InteropSubkey(PublicSecret)
	for _, a := range browserSlotTokens {
		if !f.Contains(InteropKey(sub, 1, common.HexToAddress(a))) {
			t.Errorf("FALSE NEGATIVE on %s: the browser's builder and this reader disagree", a)
		}
	}

	// The same addresses keyed for another chain have to miss, or the chain is not
	// reaching the preimage and the filter is answering for everywhere at once. A
	// bloom can say yes by accident, so allow the measured rate a wide margin rather
	// than demanding every one of them miss.
	hits := 0
	for _, a := range browserSlotTokens {
		if f.Contains(InteropKey(sub, 8453, common.HexToAddress(a))) {
			hits++
		}
	}
	if hits > len(browserSlotTokens)/3 {
		t.Errorf("%d of %d addresses hit when keyed for Base; the chain is not in the preimage",
			hits, len(browserSlotTokens))
	}

	// Half the bits set is what a correctly sized bloom looks like. Far from it in
	// either direction means the builder and the sizing rule have come apart.
	if s := f.Saturation(); s < 0.25 || s > 0.75 {
		t.Errorf("saturation %.0f%%, want near 50%% for a filter sized to its key count", 100*s)
	}
}
