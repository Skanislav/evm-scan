package hintfilter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// SnapshotFunc returns the index as of toBlock; RangeFunc returns how far the
// chain's coverage currently reaches.
//
// These are function values rather than an interface for the same reason
// hintreg.Mirror takes HeadFunc and CodeFunc: it lets a caller reach into the store
// without this package importing it, which keeps a pure data structure — one whose
// other implementation runs in a browser — free of a database driver. The caller
// does the small conversion, visibly, at the wiring point.
type (
	SnapshotFunc func(ctx context.Context, toBlock uint64) ([]AccountAssetSet, error)
	RangeFunc    func(ctx context.Context) (from, to uint64, err error)
)

// AccountAssetSet mirrors store.AccountAssetSet.
type AccountAssetSet struct {
	Account common.Address
	Assets  []common.Address
}

// FromIndex builds the index filter over an index snapshot's (account, token) pairs.
//
// EpochID is deliberately left at -1 even when the caller is building for a known
// epoch. The publisher fixes this filter's digest while composing the epoch, before
// the database has assigned the epoch an id and long before the chain has; writing
// the id into the header would mean the digest could not be computed until after the
// thing that records it already existed. The binding runs the other way instead —
// the epoch row names its filter's digest — and ToBlock ties the two to one block.
func FromIndex(chainID uint64, sets []AccountAssetSet, toBlock uint64) (*Filter, []byte, Manifest, error) {
	if len(sets) == 0 {
		return nil, nil, Manifest{}, ErrEmpty
	}
	sub := Subkey(PublicSecret, chainID, KindAccountToken)
	keys := make([]uint64, 0, len(sets)*3)
	for _, s := range sets {
		for _, a := range s.Assets {
			keys = append(keys, PairKey(sub, s.Account, a))
		}
	}
	f, err := Build(keys, Meta{
		ChainID: chainID, Kind: KindAccountToken, EpochID: -1, ToBlock: toBlock,
	})
	if err != nil {
		return nil, nil, Manifest{}, err
	}
	enc, err := f.Encode()
	if err != nil {
		return nil, nil, Manifest{}, err
	}
	return f, enc, BuildManifest(f, enc, fmt.Sprintf("index chain %d", chainID), ""), nil
}

// Cache serves the index filter for one chain, rebuilding it when the chain's
// coverage has moved on.
//
// It is deliberately not hung off the publisher. A filter is useful to any reader
// of any deployment, including one with no publisher key configured and therefore
// no epochs at all, and tying the artifact to the commitment would have meant those
// deployments silently serve nothing.
type Cache struct {
	snapshot SnapshotFunc
	coverage RangeFunc
	chainID  uint64

	// MinInterval bounds how often a rebuild can happen. SnapshotIndex
	// materialises the whole index in memory, so this is the difference between a
	// background cost and a way to make the daemon do it once per request.
	MinInterval time.Duration

	// static marks a filter that is loaded once and never rebuilt — a token list,
	// which changes when someone publishes a new version of it, not when the chain
	// moves.
	static bool

	mu       sync.Mutex
	filter   *Filter
	encoded  []byte
	manifest Manifest
	builtAt  time.Time
	builtTo  uint64
}

// NewStatic wraps an already-built filter in the same type, so a caller that holds
// a mix of static and rebuilt filters does not have to branch on which is which.
// Get never rebuilds and never fails.
func NewStatic(f *Filter, m Manifest, encoded []byte) *Cache {
	return &Cache{filter: f, encoded: encoded, manifest: m, static: true}
}

// NewCache returns a cache for one chain.
func NewCache(chainID uint64, snapshot SnapshotFunc, coverage RangeFunc) *Cache {
	return &Cache{chainID: chainID, snapshot: snapshot, coverage: coverage, MinInterval: 5 * time.Minute}
}

// Get returns the current index filter, building or refreshing it if needed.
//
// The returned bytes are shared and must not be modified: they are handed to every
// caller and written straight to HTTP responses.
func (c *Cache) Get(ctx context.Context) (*Filter, []byte, Manifest, error) {
	c.mu.Lock()
	if c.static {
		defer c.mu.Unlock()
		return c.filter, c.encoded, c.manifest, nil
	}
	cached, enc, man, builtTo, builtAt := c.filter, c.encoded, c.manifest, c.builtTo, c.builtAt
	c.mu.Unlock()

	_, to, err := c.coverage(ctx)
	if err != nil {
		if cached != nil {
			// A momentary database problem should not blank a filter that is
			// merely a few blocks old.
			return cached, enc, man, nil
		}
		return nil, nil, Manifest{}, err
	}

	// Rebuild only when the index has actually advanced past what the cached filter
	// covers. A filter that is behind is not merely old, it hides holdings: a pair
	// indexed after the build is a false negative, which is the one failure mode
	// this structure otherwise cannot have.
	if cached != nil && (builtTo >= to || time.Since(builtAt) < c.MinInterval) {
		return cached, enc, man, nil
	}

	// Built without the lock held. SnapshotIndex materialises the whole index, which
	// takes seconds on a real chain, and holding the mutex across it would stall
	// every concurrent reader — including the portfolio path, which asks for the
	// token filter on its way through. Two goroutines racing here both build and
	// the later write wins, which costs some duplicated work exactly once per
	// advance and is far cheaper than the stall.
	sets, err := c.snapshot(ctx, to)
	if err != nil {
		if cached != nil {
			return cached, enc, man, nil
		}
		return nil, nil, Manifest{}, err
	}
	if len(sets) == 0 {
		return nil, nil, Manifest{}, ErrEmpty
	}

	f, newEnc, newMan, err := FromIndex(c.chainID, sets, to)
	if err != nil {
		return nil, nil, Manifest{}, err
	}

	c.mu.Lock()
	// Another goroutine may have finished a newer build while this one ran. Keep
	// whichever covers more; going backwards would reintroduce the false negatives
	// the rebuild existed to remove.
	if c.filter == nil || to >= c.builtTo {
		c.filter, c.encoded, c.manifest = f, newEnc, newMan
		c.builtAt, c.builtTo = time.Now(), to
	}
	f, newEnc, newMan = c.filter, c.encoded, c.manifest
	c.mu.Unlock()
	return f, newEnc, newMan, nil
}
