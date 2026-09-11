package hintfilter

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// keyset builds n deterministic keys. Deliberately spread across the whole 64-bit
// range: the fuse lookup derives its segment indices from a 64-bit multiply, and a
// reader that drops the high bits (the obvious way to get this wrong in JavaScript)
// still passes a test whose keys are all small.
func keyset(n int) []uint64 {
	sub := Subkey(PublicSecret, 1, KindToken)
	out := make([]uint64, n)
	for i := range out {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(i))
		out[i] = Key(sub, b[:])
	}
	return out
}

func TestBuildNoFalseNegatives(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want Structure
	}{
		{"small picks sorted", 100, StructureSortedU64},
		{"large picks fuse", 50_000, StructureFuse8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := keyset(tc.n)
			probe := append([]uint64(nil), keys...)

			f, err := Build(keys, Meta{ChainID: 1, Kind: KindToken})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if f.Structure != tc.want {
				t.Errorf("structure = %v, want %v", f.Structure, tc.want)
			}
			if f.Count() != uint64(tc.n) {
				t.Errorf("Count() = %d, want %d", f.Count(), tc.n)
			}
			for i, k := range probe {
				if !f.Contains(k) {
					t.Fatalf("key %d (%#x) missing: a filter may never have a false negative", i, k)
				}
			}
		})
	}
}

func TestFalsePositiveRate(t *testing.T) {
	const n = 100_000
	f, err := Build(keyset(n), Meta{ChainID: 1, Kind: KindToken})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Absent keys come from a different subkey, so they are independent of the set
	// without being structured differently from it.
	sub := Subkey([]byte("absent"), 1, KindToken)
	const trials = 200_000
	hits := 0
	for i := range trials {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(i))
		if f.Contains(Key(sub, b[:])) {
			hits++
		}
	}
	rate := float64(hits) / trials
	if rate > 0.01 {
		t.Errorf("false positive rate %.4f, want the ~0.004 a fuse8 filter should give", rate)
	}
	t.Logf("fuse8 over %d keys: %d bytes, %.4f false positive rate", n, mustEncodeLen(t, f), rate)
}

func mustEncodeLen(t *testing.T, f *Filter) int {
	t.Helper()
	b, err := f.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return len(b)
}

// TestDeterministic is the property the whole format rests on: a published filter
// is only checkable if rebuilding it from the same inputs gives the same bytes.
func TestDeterministic(t *testing.T) {
	for _, n := range []int{100, 50_000} {
		meta := Meta{ChainID: 1, Kind: KindToken, EpochID: 7, ToBlock: 1234}
		a, err := Build(keyset(n), meta)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		b, err := Build(keyset(n), meta)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		ab, bb := mustEncode(t, a), mustEncode(t, b)
		if !bytes.Equal(ab, bb) {
			t.Fatalf("n=%d: two builds of the same keys produced different bytes", n)
		}
	}
}

func mustEncode(t *testing.T, f *Filter) []byte {
	t.Helper()
	b, err := f.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{1, 100, 50_000} {
		keys := keyset(n)
		probe := append([]uint64(nil), keys...)
		desc := []byte(`{"kdf":"webauthn-prf","cred_id":"abc","input":"def"}`)

		f, err := Build(keys, Meta{
			Blinded: true, ChainID: 8453, Kind: KindAccountToken,
			EpochID: 42, ToBlock: 99, Desc: desc,
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		got, err := Decode(mustEncode(t, f))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !got.Blinded || got.ChainID != 8453 || got.Kind != KindAccountToken ||
			got.EpochID != 42 || got.ToBlock != 99 || !bytes.Equal(got.Desc, desc) {
			t.Errorf("n=%d: metadata did not survive the round trip: %+v", n, got.Meta)
		}
		if got.Count() != uint64(n) {
			t.Errorf("n=%d: Count() = %d", n, got.Count())
		}
		for _, k := range probe {
			if !got.Contains(k) {
				t.Fatalf("n=%d: key %#x missing after decode", n, k)
			}
		}

		d, err := got.Descriptor()
		if err != nil || d == nil || d.KDF != "webauthn-prf" {
			t.Errorf("n=%d: Descriptor() = %+v, %v", n, d, err)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	good := mustEncode(t, mustBuild(t, keyset(100), Meta{ChainID: 1, Kind: KindToken}))

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", good[:10]},
		{"bad magic", append([]byte("NOPE"), good[4:]...)},
		{"truncated body", good[:len(good)-8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.in); err == nil {
				t.Error("Decode accepted a malformed file")
			}
		})
	}

	t.Run("bad version", func(t *testing.T) {
		b := append([]byte(nil), good...)
		b[4] = 99
		if _, err := Decode(b); err == nil {
			t.Error("Decode accepted an unknown version")
		}
	})

	// A claimed count far past the bytes present must not be believed. A length
	// prefix is an instruction to allocate.
	t.Run("lying count", func(t *testing.T) {
		b := append([]byte(nil), good...)
		binary.BigEndian.PutUint64(b[headerFixed:], 1<<40)
		if _, err := Decode(b); err == nil {
			t.Error("Decode believed a count the file could not contain")
		}
	})
}

func mustBuild(t *testing.T, keys []uint64, m Meta) *Filter {
	t.Helper()
	f, err := Build(keys, m)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f
}

func TestBuildEmpty(t *testing.T) {
	if _, err := Build(nil, Meta{}); err != ErrEmpty {
		t.Errorf("Build(nil) = %v, want ErrEmpty", err)
	}
}

func TestBuildDedupes(t *testing.T) {
	k := keyset(10)
	dup := append(append([]uint64(nil), k...), k...)
	f := mustBuild(t, dup, Meta{ChainID: 1, Kind: KindToken})
	if f.Count() != 10 {
		t.Errorf("Count() = %d over 20 keys with 10 distinct, want 10", f.Count())
	}
}

// TestSubkeyDomainSeparation is what stops a filter built for one chain or one kind
// from being readable with the subkey minted for another.
func TestSubkeyDomainSeparation(t *testing.T) {
	base := Subkey([]byte("s"), 1, KindToken)
	for _, other := range [][32]byte{
		Subkey([]byte("s"), 2, KindToken),
		Subkey([]byte("s"), 1, KindAccountToken),
		Subkey([]byte("t"), 1, KindToken),
	} {
		if base == other {
			t.Error("subkeys collided across chain, kind or secret")
		}
	}
}

func TestBlindingHidesMembership(t *testing.T) {
	token := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	mine := Subkey([]byte("my passkey output"), 1, KindToken)
	theirs := Subkey([]byte("someone else"), 1, KindToken)

	f := mustBuild(t, []uint64{TokenKey(mine, token)}, Meta{Blinded: true, ChainID: 1, Kind: KindToken})

	if !f.Contains(TokenKey(mine, token)) {
		t.Error("the secret holder cannot read their own filter")
	}
	// Someone holding the file and the whole public token dictionary, but not the
	// secret, learns nothing: every key they can derive is unrelated to the ones
	// in the file.
	if f.Contains(TokenKey(theirs, token)) {
		t.Error("a filter blinded under one secret answered to another")
	}
}
