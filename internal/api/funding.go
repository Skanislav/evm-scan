package api

import (
	"context"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// How the registry's per-asset funding reaches a response.
//
// getFunding is one eth_call per asset, and through a light client every eth_call
// is verified with eth_getProof — seconds each, not milliseconds. The asset list
// used to make that call for every row, in series, on every request, and the page
// polls it every five seconds. At 166 assets the list never returned inside any
// client's patience, and everything on the page that needs the asset list — the
// private lookup first among them — saw an empty set and said so.
//
// So the registry is asked in the background, a few assets at a time, and a list
// answers from what is known. A row whose funding has not been read yet carries no
// funding fields, which sorts it as if nobody paid; the next poll fills it in.
// Routes about a single asset can afford the call and still make it.
const (
	// fundingTTL is how long a reading stands before it is asked for again.
	// Funding moves when someone pays or a publisher claims, both rare.
	fundingTTL = 5 * time.Minute
	// fundingWorkers bounds the eth_calls in flight, so a list of hundreds does
	// not become hundreds of proofs the light client has to fetch at once.
	fundingWorkers = 4
	// fundingCallTimeout is the patience for one background read.
	fundingCallTimeout = 60 * time.Second
)

type fundingEntry struct {
	f  hintreg.Funding
	at time.Time
}

type fundingCache struct {
	mu       sync.Mutex
	got      map[common.Hash]fundingEntry
	inflight map[common.Hash]bool
	sem      chan struct{}
	// now is the clock, swapped in tests.
	now func() time.Time
}

func newFundingCache() *fundingCache {
	return &fundingCache{
		got:      map[common.Hash]fundingEntry{},
		inflight: map[common.Hash]bool{},
		sem:      make(chan struct{}, fundingWorkers),
		now:      time.Now,
	}
}

// get returns what is known about key, and whether anything is. If the reading is
// missing or stale it starts a refresh — unless one is already running, or every
// worker is busy, in which case the next request asks again. It never waits on
// the chain.
func (c *fundingCache) get(reg *hintreg.Client, key common.Hash) (hintreg.Funding, bool) {
	c.mu.Lock()
	e, ok := c.got[key]
	fresh := ok && c.now().Sub(e.at) < fundingTTL
	start := !fresh && !c.inflight[key]
	if start {
		select {
		case c.sem <- struct{}{}:
			c.inflight[key] = true
		default:
			start = false // the pool is full; the next request will try again
		}
	}
	c.mu.Unlock()

	if start {
		go c.refresh(reg, key)
	}
	return e.f, ok
}

func (c *fundingCache) refresh(reg *hintreg.Client, key common.Hash) {
	defer func() {
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		<-c.sem
	}()
	ctx, cancel := context.WithTimeout(context.Background(), fundingCallTimeout)
	defer cancel()
	f, err := reg.Funding(ctx, key)
	if err != nil {
		return // whatever was known stands; the next request retries
	}
	c.put(key, f)
}

// fetch reads one asset now, on the caller's context, and remembers the answer.
// For the routes that are about one asset and can afford one verified call.
func (c *fundingCache) fetch(ctx context.Context, reg *hintreg.Client, key common.Hash) (hintreg.Funding, error) {
	f, err := reg.Funding(ctx, key)
	if err != nil {
		return f, err
	}
	c.put(key, f)
	return f, nil
}

func (c *fundingCache) put(key common.Hash, f hintreg.Funding) {
	c.mu.Lock()
	c.got[key] = fundingEntry{f: f, at: c.now()}
	c.mu.Unlock()
}
