package ens

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/ccip"
)

func TestNormalizeRejects(t *testing.T) {
	for _, bad := range []string{"", ".", "a..eth", "a b.eth", "a\tb.eth", "a .eth", strings.Repeat("a", 64) + ".eth"} {
		if _, err := Normalize(bad); err == nil {
			t.Fatalf("Normalize(%q) should fail", bad)
		}
	}
	if _, err := Normalize(""); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("empty name: %v", err)
	}
	got, err := Normalize(" Vitalik.ETH. ")
	if err != nil || got != "vitalik.eth" {
		t.Fatalf("trailing dot, space and case: %q %v", got, err)
	}
	// Composed and decomposed é normalize to the same name.
	a, _ := Normalize("café.eth")
	b, _ := Normalize("café.eth")
	if a != b {
		t.Fatalf("NFC: %q != %q", a, b)
	}
	// A normalized name is always DNS-encodable: every label fits its length byte.
	if b := DNSEncode(got); string(b) != "\x07vitalik\x03eth\x00" {
		t.Fatalf("DNSEncode = %q", b)
	}
}

func TestSelectorsDerivedFromSignatures(t *testing.T) {
	cases := map[string][]byte{
		"3b3b57de": selAddr,
		"59d1d43c": selText,
		"77209fe8": errResolverNotFound[:],
		"556f1830": ccip.OffchainLookupSelector[:],
	}
	for want, got := range cases {
		if hex.EncodeToString(got) != want {
			t.Fatalf("selector %x, want %s", got, want)
		}
	}
}

func TestHintNameRoundTrip(t *testing.T) {
	a := common.HexToAddress("0xAbCdEf0123456789abcdef0123456789ABCDEF01")
	plain := HintName(a, "evmscan.eth", 1, false)
	if plain != "abcdef0123456789abcdef0123456789abcdef01.hints.evmscan.eth" {
		t.Fatalf("HintName = %q", plain)
	}
	withChain := HintName(a, "evmscan.eth", 8453, true)
	if withChain != "abcdef0123456789abcdef0123456789abcdef01.8453.hints.evmscan.eth" {
		t.Fatalf("HintName with chain = %q", withChain)
	}

	acct, id, has, ok := ParseHintName(plain)
	if !ok || has || acct != a {
		t.Fatalf("ParseHintName(plain) = %s %d %v %v", acct.Hex(), id, has, ok)
	}
	acct, id, has, ok = ParseHintName(strings.ToUpper(withChain))
	if !ok || !has || id != 8453 || acct != a {
		t.Fatalf("ParseHintName(chain) = %s %d %v %v", acct.Hex(), id, has, ok)
	}
	if _, _, _, ok := ParseHintName("vitalik.eth"); ok {
		t.Fatal("a name whose first label is not an address is not a hint name")
	}
}

func TestSplitContracts(t *testing.T) {
	got, err := SplitContracts(" 0x000000000000000000000000000000000000dEaD, 0x0000000000000000000000000000000000000001 ")
	if err != nil || len(got) != 2 || got[0] != common.HexToAddress("0xdead") {
		t.Fatalf("SplitContracts = %v %v", got, err)
	}
	if got, err := SplitContracts(""); err != nil || got != nil {
		t.Fatalf("empty record: %v %v", got, err)
	}
	if _, err := SplitContracts("0x1,0x2"); err == nil {
		t.Fatal("a non-address entry should fail")
	}
}

func TestClassifyRevert(t *testing.T) {
	if err := classifyRevert(errResolverNotFound[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolverNotFound → %v", err)
	}
	body, _ := strBytesErr.Pack("x.eth", []byte{1})
	var mm *ErrReverseMismatch
	if err := classifyRevert(append(errReverseMismatch[:], body...)); !errors.As(err, &mm) || mm.Primary != "x.eth" {
		t.Fatalf("ReverseAddressMismatch → %v", err)
	}
	if err := classifyRevert([]byte{1, 2}); err == nil {
		t.Fatal("short revert data should still be an error")
	}
}
