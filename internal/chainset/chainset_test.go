package chainset

import (
	"sync"
	"testing"
)

func entry(id uint64) *Entry { return &Entry{ID: id, Name: "c"} }

// TestOrderIsInsertionOrder pins the property the API's default chain rests on.
// The four maps this replaced could not answer "which chain is this deployment
// mainly about" at all, which is why there was a separate ChainOrder slice; if
// order stops being insertion order, every request without a chain_id silently
// starts answering about a different chain.
func TestOrderIsInsertionOrder(t *testing.T) {
	s := New()
	for _, id := range []uint64{1, 8453, 42161} {
		if err := s.Add(entry(id)); err != nil {
			t.Fatalf("Add(%d): %v", id, err)
		}
	}

	want := []uint64{1, 8453, 42161}
	for i := 0; i < 50; i++ { // a map range would vary between iterations
		got := s.IDs()
		if len(got) != len(want) {
			t.Fatalf("IDs() = %v", got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("IDs() = %v, want %v", got, want)
			}
		}
	}

	first, ok := s.First()
	if !ok || first.ID != 1 {
		t.Errorf("First() = %v, %v; want chain 1", first, ok)
	}

	// A chain added later must not displace the configured first one — that is
	// what keeps `POST /v1/chains` from changing what every existing query means.
	if err := s.Add(entry(10)); err != nil {
		t.Fatal(err)
	}
	if first, _ := s.First(); first.ID != 1 {
		t.Errorf("adding a chain moved the default to %d", first.ID)
	}
}

// TestAddRefusesDuplicates: two entries for one chain would be two indexers
// writing the same cursors.
func TestAddRefusesDuplicates(t *testing.T) {
	s := New()
	if err := s.Add(entry(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(entry(1)); err == nil {
		t.Error("Add accepted a duplicate chain id")
	}
	if err := s.Add(&Entry{}); err == nil {
		t.Error("Add accepted an entry with no chain id")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestRemove(t *testing.T) {
	s := New()
	for _, id := range []uint64{1, 10, 8453} {
		_ = s.Add(entry(id))
	}
	if _, ok := s.Remove(10); !ok {
		t.Fatal("Remove(10) found nothing")
	}
	if _, ok := s.Get(10); ok {
		t.Error("chain 10 is still there")
	}
	got := s.IDs()
	if len(got) != 2 || got[0] != 1 || got[1] != 8453 {
		t.Errorf("IDs after remove = %v, want [1 8453]", got)
	}
	if _, ok := s.Remove(999); ok {
		t.Error("Remove invented a chain")
	}
}

// TestNilSetIsEmpty keeps callers free of nil checks — an api.Deps built without
// chains (as the auth tests do) must not panic on a read path.
func TestNilSetIsEmpty(t *testing.T) {
	var s *Set
	if s.Len() != 0 || s.IDs() != nil || s.Entries() != nil {
		t.Error("a nil set is not behaving as empty")
	}
	if _, ok := s.Get(1); ok {
		t.Error("nil set returned a chain")
	}
	if _, ok := s.First(); ok {
		t.Error("nil set has a first chain")
	}
	if _, ok := s.Source(1); ok {
		t.Error("nil set returned a source")
	}
	if _, ok := s.Worker(1); ok {
		t.Error("nil set returned a worker")
	}
	if _, ok := s.Pricer(1); ok {
		t.Error("nil set returned a pricer")
	}
	s.CloseAll() // must not panic
}

// TestAbsentWorkerAndPricer: the registry's own chain is dialled but not indexed,
// and a chain with no on-chain price sources has no pricer. Both are ordinary.
func TestAbsentWorkerAndPricer(t *testing.T) {
	s := New()
	_ = s.Add(&Entry{ID: 8453, Name: "base"})
	if _, ok := s.Get(8453); !ok {
		t.Fatal("chain missing")
	}
	if _, ok := s.Worker(8453); ok {
		t.Error("Worker returned ok for a chain with no indexer")
	}
	if _, ok := s.Pricer(8453); ok {
		t.Error("Pricer returned ok for a chain with no pricer")
	}
}

// TestConcurrentAccess is the whole reason this type exists rather than four
// maps. Run under -race.
func TestConcurrentAccess(t *testing.T) {
	s := New()
	_ = s.Add(entry(1))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch j % 4 {
				case 0:
					_ = s.IDs()
				case 1:
					_, _ = s.Get(1)
				case 2:
					_ = s.Entries()
				case 3:
					_ = s.Len()
				}
			}
		}(i)
	}
	// Writers racing the readers above: this is exactly the pattern that adding
	// a chain over HTTP creates while requests are in flight.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := uint64(1000 + i)
			for j := 0; j < 50; j++ {
				_ = s.Add(entry(id))
				s.Remove(id)
			}
		}(i)
	}
	wg.Wait()
}
