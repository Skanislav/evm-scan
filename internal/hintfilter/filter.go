package hintfilter

import (
	"errors"
	"fmt"
	"sort"

	"github.com/FastFilter/xorfilter"
)

// Structure is how a filter stores its keys.
//
// There are two because construction cost and size pull in opposite directions at
// different scales. A binary-fuse filter is about 1.13 bytes per key, but building
// one runs a peeling loop with retries — worth writing once in Go for a published
// artifact with millions of keys, not worth reimplementing in a browser for a
// watchlist of two hundred. At that size the compression saves a few kilobytes and
// costs a second implementation of a subtle algorithm, which is a bad trade.
type Structure uint8

const (
	// StructureFuse8 is a binary-fuse8 filter: ~1.13 bytes/key, ~0.4% false
	// positives, built in Go only.
	StructureFuse8 Structure = 1
	// StructureSortedU64 is a sorted array of the truncated keys: 8 bytes/key, no
	// false positives beyond the 64-bit truncation itself, and buildable anywhere
	// in ten lines.
	StructureSortedU64 Structure = 2
)

func (s Structure) String() string {
	switch s {
	case StructureFuse8:
		return "binary-fuse8"
	case StructureSortedU64:
		return "sorted-u64"
	}
	return "unknown"
}

// autoStructureMax is the key count below which Build picks sorted-u64 on its own.
// At 4096 keys sorted-u64 costs 32 KB against roughly 4.6 KB for a fuse filter;
// under that the difference stops mattering next to the convenience of a structure
// every reader can also write.
const autoStructureMax = 4096

// ErrEmpty is returned when there is nothing to build a filter over. It is a
// distinct error because an empty filter and a filter that failed to build are very
// different things to a caller, and only one of them is a bug.
var ErrEmpty = errors.New("hintfilter: no keys")

// Meta is everything about a filter except its keys.
type Meta struct {
	// Structure selects the encoding; zero means pick one by size.
	Structure Structure
	// Blinded records that the keys were derived under a reader's secret. It does
	// not change how the file is read — that is the point of blinding, the file
	// looks the same either way — but a reader needs to know to ask for a secret
	// before testing anything against it.
	//
	// Nothing here can check this against the keys, because by the time Build sees
	// them they are opaque 64-bit values. Keeping it true is the caller's job:
	// set it exactly when the subkey came from a non-empty secret. A file that
	// gets this wrong is not corrupt, it is worse — it reads as public and then
	// answers no to everything.
	Blinded bool
	ChainID uint64
	Kind    Kind
	// EpochID and ToBlock say how current the filter is, and exist because "a
	// filter never lies, it is only slow" holds solely for keys present at build
	// time. A pair indexed after the build is a real false negative — a holding the
	// reader will not be shown — so an index-derived filter has to carry the block
	// it is true as of, and the reader has to display it.
	EpochID int64
	ToBlock uint64
	// Desc says how to re-derive the blinding secret, and never what it is. See
	// SaltDesc.
	Desc []byte
}

// Filter is a membership test over 64-bit keys, plus the metadata that says what
// the keys mean and how current they are.
type Filter struct {
	Meta
	count uint64

	fuse   *xorfilter.BinaryFuse[uint8]
	sorted []uint64
}

// Build constructs a filter over keys. It may reorder and dedupe keys in place.
func Build(keys []uint64, meta Meta) (*Filter, error) {
	if len(keys) == 0 {
		return nil, ErrEmpty
	}

	// Duplicates are normal input here — the same token appears on several lists,
	// and an account touches the same contract repeatedly — but the fuse builder
	// only tolerates so many before construction stops converging, so dedupe up
	// front rather than leaving it to chance.
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	n := 0
	for i, k := range keys {
		if i == 0 || k != keys[n-1] {
			keys[n] = k
			n++
		}
	}
	keys = keys[:n]

	if meta.Structure == 0 {
		if len(keys) <= autoStructureMax {
			meta.Structure = StructureSortedU64
		} else {
			meta.Structure = StructureFuse8
		}
	}

	f := &Filter{Meta: meta, count: uint64(len(keys))}
	switch meta.Structure {
	case StructureSortedU64:
		f.sorted = keys
	case StructureFuse8:
		fuse, err := xorfilter.NewBinaryFuse[uint8](keys)
		if err != nil {
			return nil, fmt.Errorf("hintfilter: build fuse over %d keys: %w", len(keys), err)
		}
		f.fuse = fuse
	default:
		return nil, fmt.Errorf("hintfilter: unknown structure %d", meta.Structure)
	}
	return f, nil
}

// Count is how many distinct keys the filter holds.
//
// Worth knowing that this is not private: it is in the header and it follows from
// the file length anyway. A blinded filter hides which addresses are in it, never
// how many.
func (f *Filter) Count() uint64 { return f.count }

// Contains reports whether key is in the set, with a false positive probability of
// roughly 0.4% for StructureFuse8 and effectively zero for StructureSortedU64.
//
// There are no false negatives for a key that was present at build time, which is
// what lets callers treat a miss as decisive and a hit as a reason to go look.
func (f *Filter) Contains(key uint64) bool {
	switch f.Structure {
	case StructureSortedU64:
		i := sort.Search(len(f.sorted), func(i int) bool { return f.sorted[i] >= key })
		return i < len(f.sorted) && f.sorted[i] == key
	case StructureFuse8:
		return f.fuse.Contains(key)
	}
	return false
}
