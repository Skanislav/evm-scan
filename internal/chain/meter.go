package chain

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Meter counts RPC calls by method.
//
// It exists because a metered provider bills per request, so the only lever on
// what a deployment costs is how many requests it makes — and until this, nobody
// could say. Measured on dRPC, every method billed a flat 20 compute units, which
// makes request count the whole story rather than a proxy for it.
//
// What it can and cannot see is worth stating plainly, because getting it wrong
// would produce a confident wrong number. This counts calls *this process makes to
// the endpoint it dialled*. When that endpoint is a light client on loopback, the
// provider bills for what the light client fetches upstream to verify an answer,
// which is several requests per call and invisible from here. Only a chain dialled
// directly has counts that are also the bill; MeteredSource.BilledDirectly says
// which kind a chain is, and nothing here pretends otherwise.
type Meter struct {
	mu     sync.RWMutex
	counts map[string]*atomic.Uint64
}

func NewMeter() *Meter { return &Meter{counts: map[string]*atomic.Uint64{}} }

func (m *Meter) add(method string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	c, ok := m.counts[method]
	m.mu.RUnlock()
	if ok {
		c.Add(1)
		return
	}

	m.mu.Lock()
	if c, ok = m.counts[method]; !ok {
		c = new(atomic.Uint64)
		m.counts[method] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

// Snapshot returns per-method counts and their total.
func (m *Meter) Snapshot() (map[string]uint64, uint64) {
	if m == nil {
		return nil, 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string]uint64, len(m.counts))
	var total uint64
	for k, c := range m.counts {
		v := c.Load()
		out[k] = v
		total += v
	}
	return out, total
}

// Methods lists the counted methods in descending order of use, which is the order
// anyone reading a bill wants them in.
func (m *Meter) Methods() []string {
	counts, _ := m.Snapshot()
	out := make([]string, 0, len(counts))
	for k := range counts {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// MeteredSource is a Source that counts what it has asked its endpoint for. It is a
// separate interface rather than part of Source so that the many fakes in tests do
// not have to grow a method they have no use for.
type MeteredSource interface {
	Source
	RPCCalls() (byMethod map[string]uint64, total uint64)
	// BilledDirectly reports whether these counts are what a provider bills for.
	// False means the endpoint is a local light client, whose upstream fetches are
	// the billed traffic and are several times this.
	BilledDirectly() bool
}

func (n *Node) RPCCalls() (map[string]uint64, uint64) { return n.meter.Snapshot() }

// BilledDirectly is true for a remote endpoint, where this process is the only
// thing talking to the provider.
func (n *Node) BilledDirectly() bool { return !n.ep.Local }
