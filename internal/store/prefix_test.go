package store

import (
	"encoding/hex"
	"testing"
)

func TestAccountPrefixRange(t *testing.T) {
	cases := []struct {
		name, in, lo, hi string
		ok               bool
	}{
		{"empty means no filter", "", "", "", true},
		{"bare 0x means no filter", "0x", "", "", true},
		{"one nibble", "a",
			"a000000000000000000000000000000000000000",
			"afffffffffffffffffffffffffffffffffffffff", true},
		{"even nibbles", "0xab",
			"ab00000000000000000000000000000000000000",
			"abffffffffffffffffffffffffffffffffffffff", true},
		{"odd nibbles hold the last one fixed", "0xabc",
			"abc0000000000000000000000000000000000000",
			"abcfffffffffffffffffffffffffffffffffffff", true},
		{"uppercase prefix", "0XAB",
			"ab00000000000000000000000000000000000000",
			"abffffffffffffffffffffffffffffffffffffff", true},
		{"a whole address pins one row",
			"0x073D894Fb68a6450dA4Ae6019936D14ef9144479",
			"073d894fb68a6450da4ae6019936d14ef9144479",
			"073d894fb68a6450da4ae6019936d14ef9144479", true},
		{"longer than an address", "0x" + "ab" + "00000000000000000000000000000000000000000", "", "", false},
		{"not hex", "0xzz", "", "", false},
		{"not hex mid-string", "0xabg", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lo, hi, ok := AccountPrefixRange(c.in)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if c.lo == "" {
				if lo != nil || hi != nil {
					t.Fatalf("want no bounds, got %x..%x", lo, hi)
				}
				return
			}
			// Both bounds must be a whole address, or the BYTEA comparison in the query
			// silently compares different-length values.
			if len(lo) != 20 || len(hi) != 20 {
				t.Fatalf("bounds must be 20 bytes, got %d and %d", len(lo), len(hi))
			}
			if got := hex.EncodeToString(lo); got != c.lo {
				t.Errorf("lo = %s, want %s", got, c.lo)
			}
			if got := hex.EncodeToString(hi); got != c.hi {
				t.Errorf("hi = %s, want %s", got, c.hi)
			}
		})
	}
}
