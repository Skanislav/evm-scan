package hintfilter

import (
	"encoding/hex"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestInterop7930 pins the preimage, because two encoders that disagree about it
// produce different keys for the same contract and the filter then answers no to
// something the reader is holding.
func TestInterop7930(t *testing.T) {
	usdc := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	for _, c := range []struct {
		chainID uint64
		want    string
	}{
		// version 0001, EVM 0000, then minimal big-endian chain ref, then 0x14 and
		// the twenty address bytes. Chain 1 is one byte; 8453 and 42161 are two.
		{1, "00010000010114a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"},
		{8453, "0001000002210514a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"},
		{42161, "0001000002a4b114a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"},
	} {
		got := hex.EncodeToString(Interop7930(c.chainID, usdc))
		if got != c.want {
			t.Errorf("chain %d:\n got %s\nwant %s", c.chainID, got, c.want)
		}
	}
}

// The property the whole kind exists for: one filter, many chains, and the same
// token address on two chains is two different keys.
func TestInteropKeysAreChainScoped(t *testing.T) {
	usdc := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	sub := InteropSubkey(PublicSecret)

	mainnet := InteropKey(sub, 1, usdc)
	base := InteropKey(sub, 8453, usdc)
	if mainnet == base {
		t.Fatal("same key on two chains: the chain is not reaching the preimage")
	}

	f, err := Build([]uint64{mainnet}, Meta{Kind: KindInterop, Structure: StructureBloom, BloomBits: 256})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Contains(mainnet) {
		t.Error("mainnet USDC missing from a filter built over it")
	}
	// Not a guarantee — a bloom may say yes to anything — but at one key in 256 bits
	// a collision here would mean the probes are not spreading.
	if f.Contains(base) {
		t.Error("Base USDC hit a filter holding only the mainnet one")
	}
}

func TestBloomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	keys := make([]uint64, 65)
	for i := range keys {
		keys[i] = r.Uint64()
	}
	want := append([]uint64(nil), keys...)

	f, err := Build(keys, Meta{ChainID: 0, Kind: KindInterop, Structure: StructureBloom, BloomBits: 256})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The header is 34 bytes, plus 8 for the count, plus 4+1 for m and k, plus the
	// 32-byte bitmap. The bitmap is the part that has to be exactly a slot.
	if len(enc) != headerFixed+8+5+32 {
		t.Errorf("encoded length %d, want %d", len(enc), headerFixed+8+5+32)
	}

	back, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back.Structure != StructureBloom || back.Kind != KindInterop {
		t.Fatalf("metadata lost: structure=%v kind=%v", back.Structure, back.Kind)
	}
	m, k := back.BloomParams()
	if m != 256 || k == 0 {
		t.Fatalf("bloom params lost: m=%d k=%d", m, k)
	}
	for _, key := range want {
		if !back.Contains(key) {
			t.Fatalf("false negative after a round trip on %016x — impossible for a bloom", key)
		}
	}

	// Deterministic: the same keys and metadata must give the same bytes, or a
	// published digest means nothing.
	again, _ := Build(append([]uint64(nil), want...), Meta{ChainID: 0, Kind: KindInterop, Structure: StructureBloom, BloomBits: 256})
	enc2, _ := again.Encode()
	if string(enc) != string(enc2) {
		t.Error("two builds over the same keys produced different bytes")
	}

	slot, err := back.Slot()
	if err != nil {
		t.Fatalf("Slot: %v", err)
	}
	if len(slot) != 32 {
		t.Errorf("slot is %d bytes", len(slot))
	}
}

// A bloom's whole value is the rate it promises, so the rate is measured rather than
// assumed. A filter that quietly saturated would still pass every membership test
// above while being worth nothing.
func TestBloomFalsePositiveRate(t *testing.T) {
	const n = 65
	for _, tc := range []struct {
		bits uint32
		max  float64 // measured rate must stay under this
	}{
		{256, 0.20},
		{512, 0.05},
		{1024, 0.01},
	} {
		r := rand.New(rand.NewSource(int64(tc.bits)))
		keys := make([]uint64, n)
		set := make(map[uint64]bool, n)
		for i := range keys {
			keys[i] = r.Uint64()
			set[keys[i]] = true
		}
		f, err := Build(keys, Meta{Kind: KindInterop, Structure: StructureBloom, BloomBits: tc.bits})
		if err != nil {
			t.Fatal(err)
		}
		for k := range set {
			if !f.Contains(k) {
				t.Fatalf("%d bits: false negative, which cannot happen", tc.bits)
			}
		}
		const trials = 200_000
		fp := 0
		for i := 0; i < trials; i++ {
			k := r.Uint64()
			if !set[k] && f.Contains(k) {
				fp++
			}
		}
		rate := float64(fp) / trials
		if rate > tc.max {
			t.Errorf("%d bits over %d keys: %.3f%% false positives, want under %.1f%%",
				tc.bits, n, 100*rate, 100*tc.max)
		}
		if s := f.Saturation(); s > 0.95 {
			t.Errorf("%d bits: %.0f%% of bits set, which answers yes to nearly everything", tc.bits, 100*s)
		}
		t.Logf("%4d bits / %d keys: %.3f%% false positives, %.0f%% of bits set, %d bytes on the wire",
			tc.bits, n, 100*rate, 100*f.Saturation(), tc.bits/8)
	}
}

// A reader must not accept a bitmap whose length disagrees with the m it was told,
// because that is the shape a truncated response takes and it answers no to
// everything.
func TestBloomRejectsBadHeader(t *testing.T) {
	f, _ := Build([]uint64{1, 2, 3}, Meta{Kind: KindInterop, Structure: StructureBloom, BloomBits: 256})
	enc, _ := f.Encode()

	short := append([]byte(nil), enc[:len(enc)-1]...)
	if _, err := Decode(short); err == nil {
		t.Error("a bitmap one byte short was accepted")
	}

	zeroK := append([]byte(nil), enc...)
	zeroK[headerFixed+8+4] = 0
	if _, err := Decode(zeroK); err == nil {
		t.Error("k=0 was accepted, which matches everything")
	}
}
