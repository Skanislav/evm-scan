package hintfilter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// fakeIndex stands in for the store. Two counters, because the thing worth testing
// about a cache is how often it does the expensive thing.
type fakeIndex struct {
	to    uint64
	sets  []AccountAssetSet
	calls int64
}

func (f *fakeIndex) snapshot(ctx context.Context, toBlock uint64) ([]AccountAssetSet, error) {
	atomic.AddInt64(&f.calls, 1)
	return f.sets, nil
}

func (f *fakeIndex) coverage(ctx context.Context) (uint64, uint64, error) {
	return 0, atomic.LoadUint64(&f.to), nil
}

func newCacheOver(f *fakeIndex, chainID uint64) *Cache {
	c := NewCache(chainID, f.snapshot, f.coverage)
	// The interval exists to stop a rebuild per request; it would otherwise mask
	// the advance-triggered refresh this test is about.
	c.MinInterval = 0
	return c
}

func TestCacheRebuildsOnlyWhenCoverageAdvances(t *testing.T) {
	acc := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tok := common.HexToAddress("0x2222222222222222222222222222222222222222")
	f := &fakeIndex{to: 100, sets: []AccountAssetSet{{Account: acc, Assets: []common.Address{tok}}}}
	c := newCacheOver(f, 1)
	ctx := context.Background()

	filter, enc, m, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if atomic.LoadInt64(&f.calls) != 1 {
		t.Fatalf("first Get made %d snapshot calls, want 1", f.calls)
	}
	if m.ToBlock != 100 {
		t.Errorf("manifest ToBlock = %d, want 100", m.ToBlock)
	}
	if len(enc) == 0 {
		t.Error("no encoded bytes")
	}

	sub := Subkey(PublicSecret, 1, KindAccountToken)
	if !filter.Contains(PairKey(sub, acc, tok)) {
		t.Error("the indexed pair is missing from the filter")
	}
	// The reverse pair must not hit: order is part of the key, and a filter that
	// answered both ways would report holdings backwards.
	if filter.Contains(PairKey(sub, tok, acc)) {
		t.Error("pair key is not order-sensitive")
	}

	if _, _, _, err := c.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&f.calls) != 1 {
		t.Errorf("a second Get at the same coverage rebuilt: %d calls", f.calls)
	}

	// Once the chain moves on, the cached filter is not merely old — it would hide
	// any pair indexed since. It has to be rebuilt.
	atomic.StoreUint64(&f.to, 200)
	if _, _, m, err = c.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&f.calls) != 2 {
		t.Errorf("coverage advanced but the filter was not rebuilt: %d calls", f.calls)
	}
	if m.ToBlock != 200 {
		t.Errorf("manifest ToBlock = %d after the advance, want 200", m.ToBlock)
	}
}

func TestCacheEmptyIndex(t *testing.T) {
	c := newCacheOver(&fakeIndex{to: 10}, 1)
	if _, _, _, err := c.Get(context.Background()); err != ErrEmpty {
		t.Errorf("Get over an empty index = %v, want ErrEmpty", err)
	}
}

// TestCacheConcurrentGet is about the mutex, not the filter.
//
// SnapshotIndex materialises the whole index and takes seconds on a real chain. An
// earlier version held the lock across it, which meant every concurrent reader —
// including the portfolio path, which asks for a filter on its way through — blocked
// for the whole rebuild. Run with -race.
func TestCacheConcurrentGet(t *testing.T) {
	acc := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tok := common.HexToAddress("0x2222222222222222222222222222222222222222")
	f := &fakeIndex{to: 100, sets: []AccountAssetSet{{Account: acc, Assets: []common.Address{tok}}}}
	c := newCacheOver(f, 1)

	sub := Subkey(PublicSecret, 1, KindAccountToken)
	want := PairKey(sub, acc, tok)

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half the callers move the chain on, so rebuilds and reads overlap.
			if i%4 == 0 {
				atomic.AddUint64(&f.to, 10)
			}
			filter, enc, _, err := c.Get(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if len(enc) == 0 || !filter.Contains(want) {
				errs <- errors.New("Get returned a filter missing its only key")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
