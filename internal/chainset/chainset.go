// Package chainset holds the chains a process is currently running.
//
// It exists because the daemon used to keep four parallel maps — sources, indexer
// services, API workers, pricers — built once at startup and read for the life of
// the process. That is fine while the set is immutable and a data race the moment
// it is not, and the point of the work around this package is to let an operator
// add a network without a restart.
//
// Deliberately dumb. It does not dial, construct or supervise anything: it hands
// out what it was given, under a lock. Construction stays in cmd/evmscand, which is
// the only place that knows how to build an indexer — keeping it out of here is
// what lets internal/api depend on this package while still reaching the indexer
// only through the Worker interface, as it always has.
package chainset

import (
	"fmt"
	"sync"

	"context"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/price"
)

// Worker is the slice of a chain's indexer that anything outside the indexer needs.
//
// It lives here rather than in internal/api because both the API and the chain set
// speak in terms of it now. *indexer.Service satisfies it structurally, so nothing
// in the indexer has to know this interface exists.
type Worker interface {
	// Nudge asks the follower to run a tick promptly.
	Nudge()
	// Promote turns an observed contract into an indexed asset.
	Promote(ctx context.Context, addr common.Address, reason string) error
	// HistoryFloor is the oldest block this chain's node can serve logs for.
	HistoryFloor() uint64
	// DiscoveryThresholds are the activity levels at which a candidate qualifies
	// for promotion.
	DiscoveryThresholds() (minEvents, minBlocks, minVoters uint64)
	// Health reports whether the worker is still doing its job.
	Health() error
}

// Entry is one running chain.
type Entry struct {
	ID     uint64
	Name   string
	Source chain.Source
	// Worker is nil for a chain that is dialled but not indexed — the registry's
	// own chain, when the deployment does not also index it.
	Worker Worker
	// Pricer is nil where the chain has no on-chain price sources.
	Pricer *price.Pricer
}

// Set is the live collection. The zero value is not usable; call New. A nil *Set
// behaves as an empty one, so a caller holding no chains needs no special case.
type Set struct {
	mu      sync.RWMutex
	entries map[uint64]*Entry
	// order preserves insertion, because ranging a map does not and something has
	// to mean "the chain this deployment is mainly about". Config chains are added
	// first, so order[0] is the same chain across restarts; anything added at
	// runtime lands after them and cannot displace it.
	order []uint64
}

func New() *Set { return &Set{entries: map[uint64]*Entry{}} }

// Add registers a chain. A chain id already present is an error rather than a
// silent replacement: two entries for one chain would mean two indexers writing
// the same cursors.
func (s *Set) Add(e *Entry) error {
	if e == nil || e.ID == 0 {
		return fmt.Errorf("chainset: entry needs a chain id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[uint64]*Entry{}
	}
	if _, dup := s.entries[e.ID]; dup {
		return fmt.Errorf("chainset: chain %d is already running", e.ID)
	}
	s.entries[e.ID] = e
	s.order = append(s.order, e.ID)
	return nil
}

// Remove drops a chain and returns what was there. The caller owns closing it:
// this package does not know whether the source is still in use.
func (s *Set) Remove(id uint64) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	delete(s.entries, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return e, true
}

// Get returns one chain.
func (s *Set) Get(id uint64) (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	return e, ok
}

// Source is the common case of Get.
func (s *Set) Source(id uint64) (chain.Source, bool) {
	e, ok := s.Get(id)
	if !ok {
		return nil, false
	}
	return e.Source, true
}

// Worker returns a chain's indexer, if it has one.
func (s *Set) Worker(id uint64) (Worker, bool) {
	e, ok := s.Get(id)
	if !ok || e.Worker == nil {
		return nil, false
	}
	return e.Worker, true
}

// Pricer returns a chain's pricer, if it has one.
func (s *Set) Pricer(id uint64) (*price.Pricer, bool) {
	e, ok := s.Get(id)
	if !ok || e.Pricer == nil {
		return nil, false
	}
	return e.Pricer, true
}

// IDs lists the chains in insertion order. The first is what a request without an
// explicit chain_id means.
func (s *Set) IDs() []uint64 {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]uint64(nil), s.order...)
}

// Entries lists the chains in insertion order.
func (s *Set) Entries() []*Entry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.entries[id])
	}
	return out
}

// Len is how many chains are running.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// First is the chain a request without an explicit chain_id means.
func (s *Set) First() (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return nil, false
	}
	return s.entries[s.order[0]], true
}

// CloseAll closes every source and empties the set.
func (s *Set) CloseAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Source != nil {
			e.Source.Close()
		}
	}
	s.entries = map[uint64]*Entry{}
	s.order = nil
}
