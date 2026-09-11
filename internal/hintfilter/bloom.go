package hintfilter

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// A Bloom filter, for the sizes where it beats the other two.
//
// The rest of this package is built for the published index: 1.24 million pairs,
// where binary-fuse8's 1.13 bytes per key is what makes the file downloadable at
// all. A wallet is not that. A wallet is sixty-five contracts, and at sixty-five
// keys fuse8's fixed segment geometry costs 198 bytes — 3.05 bytes per key — while
// a Bloom sized for the same false-positive rate costs a third of that, and the
// sorted-u64 the browser builds today costs 562.
//
// Measured, for 65 keys:
//
//	  32 bytes   14.4% false positives
//	  64 bytes    2.7%
//	 128 bytes    0.18%
//	 198 bytes    0.39%   <- binary-fuse8, for comparison
//
// Which is the point of adding a third structure rather than tuning the two that
// exist: a per-account hint this small can be published on-chain or written to an
// ENS text record, and a reader anywhere can then skip most of a token list without
// asking anybody anything.
//
// HintBits is 128 bytes, and the reasoning is worth writing down because the obvious
// choice is 32 — one EVM storage slot. Measured false positives by wallet size:
//
//	 tokens |    32B     64B    128B
//	     25 |   1.3%   0.11%   0.02%
//	     50 |   9.1%   0.71%   0.05%
//	     65 |  13.7%    2.2%   0.12%
//	    100 |  28.4%    9.3%   0.78%
//	    200 |  54.7%   29.2%    8.9%
//
// One slot is already halfway to useless at fifty tokens, which is an ordinary
// wallet. And the saving it was chosen for does not exist: on Base at 0.01 gwei a
// cold write costs $0.0010 at 32 bytes and $0.0025 at 128, so the size is free and
// only the accuracy differs.
//
// 128 rather than 256 because past this the extra bytes buy decimal places rather
// than round trips — once false positives fall below the real holdings, the holdings
// themselves decide how many batches a sweep takes, and at 65 tokens 128B and 256B
// both cost three eth_calls. The headroom that is left goes to incremental
// additions, which is what a bloom is here for: a hint grows by OR-ing new keys in,
// and it should be able to absorb a few before anyone has to rebuild it.
//
// The rate degrades rather than breaking, and there are never false negatives, so an
// over-full filter is slow and not wrong. BloomBits picks a size for a target rate
// when a caller wants to decide for itself.

// StructureBloom is a bit array with k probes per key.
const StructureBloom Structure = 3

// HintBits is the default size of a published per-account hint: 1024 bits, 128
// bytes. See the commentary above for why this rather than one storage slot.
const HintBits uint32 = 1024

// bloom is the bit array plus the two numbers a reader needs to reproduce the
// probes. Both travel in the file: a reader that guessed k would silently compute
// different probes and report an empty wallet.
// The bitmap is a plain byte array, MSB first within each byte: bit p lives in
// byte p/8 at mask 0x80>>(p%8). Deliberately not uint64 words — the JavaScript
// reader indexes bytes, and a word-packed layout would need it to reproduce Go's
// endianness inside each word to find the same bit. One obvious layout, one way to
// be wrong, and testdata/public-bloom-interop.xorf is what catches it.
type bloom struct {
	bits []byte
	m    uint32 // bit count, always a multiple of 64
	k    uint8
}

// maxBloomBits caps a single filter at 1 MiB of bitmap. Past that the fuse filter
// is the better structure by a wide margin and this one is being misused.
const maxBloomBits = 8 << 20

// BloomBits returns the bit count that gives roughly the requested false-positive
// rate for n keys, rounded up to a multiple of 64.
//
// From the standard m = -n·ln(p) / (ln2)². Rounded up to a whole slot's worth of
// bits when it lands close, because 32 bytes is the size that can be published
// on-chain and a filter that is 34 bytes for no reason cannot.
func BloomBits(n int, p float64) uint32 {
	if n <= 0 {
		return 64
	}
	if p <= 0 || p >= 1 {
		p = 0.01
	}
	m := -float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)
	bitsN := uint32((int(m) + 63) / 64 * 64)
	if bitsN < 64 {
		bitsN = 64
	}
	if bitsN > maxBloomBits {
		bitsN = maxBloomBits
	}
	return bitsN
}

// bloomK is the probe count that minimises false positives for m bits and n keys:
// k = (m/n)·ln2, clamped to something a reader can afford to run per candidate.
func bloomK(m uint32, n int) uint8 {
	if n <= 0 {
		return 1
	}
	k := int(float64(m)/float64(n)*math.Ln2 + 0.5)
	if k < 1 {
		k = 1
	}
	if k > 24 {
		k = 24
	}
	return uint8(k)
}

func newBloom(keys []uint64, m uint32) (*bloom, error) {
	if m == 0 || m%64 != 0 {
		return nil, fmt.Errorf("hintfilter: bloom bit count %d is not a positive multiple of 64", m)
	}
	if m > maxBloomBits {
		return nil, fmt.Errorf("hintfilter: bloom bit count %d is over the %d limit", m, maxBloomBits)
	}
	b := &bloom{bits: make([]byte, m/8), m: m, k: bloomK(m, len(keys))}
	for _, key := range keys {
		b.add(key)
	}
	return b, nil
}

// probe is Kirsch-Mitzenmacher: two independent halves of the key combined to give
// k indices for the price of the one hash that produced the key. The key is already
// keccak output, so its halves are as independent as anything here needs.
//
// The modulo is by m rather than a mask because m is a multiple of 64 and not
// necessarily a power of two — sizing to a target rate matters more here than
// saving one division per probe on a loop that runs a few hundred times.
func (b *bloom) probe(key uint64, i int) uint32 {
	h1 := uint32(key)
	h2 := uint32(key >> 32)
	// h2 odd, so that stepping by it visits distinct positions rather than cycling
	// early when h2 shares a factor with m.
	return (h1 + uint32(i)*(h2|1)) % b.m
}

func (b *bloom) add(key uint64) {
	for i := 0; i < int(b.k); i++ {
		p := b.probe(key, i)
		b.bits[p/8] |= 0x80 >> (p % 8)
	}
}

func (b *bloom) contains(key uint64) bool {
	for i := 0; i < int(b.k); i++ {
		p := b.probe(key, i)
		if b.bits[p/8]&(0x80>>(p%8)) == 0 {
			return false
		}
	}
	return true
}

// bytesOut is the bitmap as it goes on the wire, which is already how it is held.
// A 256-bit filter is exactly 32 bytes and can be read as a bytes32.
func (b *bloom) bytesOut() []byte { return b.bits }

func bloomFromBytes(raw []byte, m uint32, k uint8) (*bloom, error) {
	if m == 0 || m%64 != 0 {
		return nil, fmt.Errorf("%w: bloom bit count %d is not a positive multiple of 64", ErrFormat, m)
	}
	if m > maxBloomBits {
		return nil, fmt.Errorf("%w: bloom bit count %d is over the limit", ErrFormat, m)
	}
	if k == 0 {
		return nil, fmt.Errorf("%w: bloom with zero probes matches everything", ErrFormat)
	}
	if len(raw) != int(m)/8 {
		return nil, fmt.Errorf("%w: bloom bitmap is %d bytes, want %d", ErrFormat, len(raw), m/8)
	}
	b := &bloom{bits: append([]byte(nil), raw...), m: m, k: k}
	return b, nil
}

// Saturation is the fraction of bits set, which is the one health check a reader can
// run on a Bloom filter it did not build. A filter that is nearly all ones answers
// yes to almost everything and is worse than useless — it looks like a working hint
// and costs a full sweep anyway. Anything above about 0.9 means the file was built
// for far more keys than it has bits.
func (f *Filter) Saturation() float64 {
	if f.Structure != StructureBloom || f.bloom == nil {
		return 0
	}
	set := 0
	for _, by := range f.bloom.bits {
		set += bits.OnesCount8(by)
	}
	return float64(set) / float64(f.bloom.m)
}

// BloomParams reports the bit count and probe count of a Bloom filter, so a caller
// can show what it is holding. Zero for any other structure.
func (f *Filter) BloomParams() (m uint32, k uint8) {
	if f.Structure != StructureBloom || f.bloom == nil {
		return 0, 0
	}
	return f.bloom.m, f.bloom.k
}

// ErrNotBloom is returned by Bitmap and Slot for a filter that is not a bloom.
var ErrNotBloom = errors.New("hintfilter: not a bloom filter")

// Bitmap returns the bits alone, without the .xorf header: what gets written to a
// storage word, an ENS text record, or anywhere else that stores a hint and knows
// its own m and k. A copy, so a caller cannot reach back into the filter.
func (f *Filter) Bitmap() ([]byte, error) {
	if f.Structure != StructureBloom || f.bloom == nil {
		return nil, ErrNotBloom
	}
	return append([]byte(nil), f.bloom.bytesOut()...), nil
}

// Slot returns the bitmap as a single bytes32, for the case where a hint has to fit
// one storage word. Only defined for a 256-bit bloom; HintBits is four times that,
// because one slot is already 9% false positives at fifty tokens.
func (f *Filter) Slot() ([32]byte, error) {
	var out [32]byte
	if f.Structure != StructureBloom || f.bloom == nil || f.bloom.m != 256 {
		return out, ErrNotBloom
	}
	copy(out[:], f.bloom.bytesOut())
	return out, nil
}
