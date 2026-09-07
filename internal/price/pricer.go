package price

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
)

// Config is one chain's pricing setup.
type Config struct {
	Sources Sources
	Policy  Policy
	// TWAPWindow is asked of every v3 pool. Default 30 minutes.
	TWAPWindow time.Duration
	// CacheTTL is how long a quote is reused before the lens is asked again.
	// Prices only change per block, so a TTL around the block time makes a UI
	// polling every few seconds cost one eth_call per block instead of one per
	// poll. Default 12 seconds.
	CacheTTL time.Duration
	// GasPerCall caps each staticcall inside the lens. Zero uses the contract's default.
	GasPerCall uint64
}

func (c Config) withDefaults() Config {
	if c.TWAPWindow <= 0 {
		c.TWAPWindow = 30 * time.Minute
	}
	if c.CacheTTL < 0 {
		c.CacheTTL = 0
	} else if c.CacheTTL == 0 {
		c.CacheTTL = 12 * time.Second
	}
	c.Policy = c.Policy.withDefaults()
	return c
}

// Pricer prices tokens on one chain, through one node, with a short cache.
type Pricer struct {
	src chain.Source
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	cache  map[common.Address]cached
	native *cached
}

type cached struct {
	quote   *Quote
	sources *TokenSources
	at      time.Time
}

// New builds a pricer. Sources that are empty make a pricer that prices nothing,
// which callers can check with Enabled.
func New(src chain.Source, cfg Config, log *slog.Logger) *Pricer {
	if log == nil {
		log = slog.Default()
	}
	return &Pricer{src: src, cfg: cfg.withDefaults(), log: log, cache: map[common.Address]cached{}}
}

// Enabled reports whether there is any source to read.
func (p *Pricer) Enabled() bool { return !p.cfg.Sources.Empty() }

// Sources is what this pricer reads.
func (p *Pricer) Sources() Sources { return p.cfg.Sources }

// Policy is how this pricer judges what it reads.
func (p *Pricer) Policy() Policy { return p.cfg.Policy }

// TWAPWindow is the averaging window asked of pools.
func (p *Pricer) TWAPWindow() time.Duration { return p.cfg.TWAPWindow }

// Result is a batch of quotes.
type Result struct {
	ChainID uint64
	// AsOfBlock is the block of the lens read that produced this result, or the
	// oldest cached block when everything came from cache. Each Quote carries its
	// own block, which is the one to trust for that token.
	AsOfBlock uint64
	Timestamp uint64
	Native    *Quote
	Quotes    map[common.Address]*Quote
	// Sources is the raw discovery per token, for callers that want to show it.
	Sources map[common.Address]*TokenSources
	// Calls is how many eth_calls this cost; zero means the cache answered.
	Calls  int
	Cached int
}

// Prices quotes every token, reading the lens for the ones the cache cannot answer.
func (p *Pricer) Prices(ctx context.Context, tokens []common.Address) (*Result, error) {
	if !p.Enabled() {
		return &Result{Quotes: map[common.Address]*Quote{}, Sources: map[common.Address]*TokenSources{}}, nil
	}

	now := time.Now()
	want := dedupe(tokens)
	res := &Result{Quotes: map[common.Address]*Quote{}, Sources: map[common.Address]*TokenSources{}}

	// Serve what the cache can.
	var misses []common.Address
	p.mu.Lock()
	for _, t := range want {
		if c, ok := p.cache[t]; ok && now.Sub(c.at) < p.cfg.CacheTTL {
			res.Quotes[t], res.Sources[t] = c.quote, c.sources
			res.Cached++
			continue
		}
		misses = append(misses, t)
	}
	nativeFresh := p.native != nil && now.Sub(p.native.at) < p.cfg.CacheTTL
	if nativeFresh {
		res.Native = p.native.quote
	}
	p.mu.Unlock()

	if len(misses) == 0 && nativeFresh {
		res.ChainID, res.AsOfBlock, res.Timestamp = p.cachedHeader(res)
		return res, nil
	}

	// One lens read for every miss plus the quote tokens, which routing needs
	// whether or not anyone asked about them directly.
	ask := append([]common.Address{}, misses...)
	for _, q := range p.cfg.Sources.QuoteTokens {
		ask = append(ask, q.Address)
	}
	ask = dedupe(ask)

	snap, err := Query(ctx, p.src, Request{
		Tokens:     ask,
		Sources:    p.cfg.Sources,
		TWAPWindow: uint32(p.cfg.TWAPWindow / time.Second),
		GasPerCall: p.cfg.GasPerCall,
	})
	if err != nil {
		return nil, err
	}
	resolved := Resolve(snap, p.cfg.Sources, p.cfg.Policy)

	p.mu.Lock()
	at := time.Now()
	for i := range snap.Tokens {
		t := &snap.Tokens[i]
		p.cache[t.Token] = cached{quote: resolved.Quotes[t.Token], sources: t, at: at}
	}
	p.native = &cached{quote: resolved.Native, at: at}
	p.mu.Unlock()

	for _, t := range misses {
		res.Quotes[t] = resolved.Quotes[t]
		for i := range snap.Tokens {
			if snap.Tokens[i].Token == t {
				res.Sources[t] = &snap.Tokens[i]
			}
		}
	}
	res.Native = resolved.Native
	res.ChainID, res.AsOfBlock, res.Timestamp = snap.ChainID, snap.BlockNumber, snap.Timestamp
	res.Calls = snap.Calls
	return res, nil
}

// Native prices the chain's own asset alone.
func (p *Pricer) Native(ctx context.Context) (*Quote, error) {
	r, err := p.Prices(ctx, nil)
	if err != nil {
		return nil, err
	}
	return r.Native, nil
}

func (p *Pricer) cachedHeader(res *Result) (chainID, block, ts uint64) {
	for _, q := range res.Quotes {
		if q == nil {
			continue
		}
		if block == 0 || q.AsOfBlock < block {
			block, ts = q.AsOfBlock, q.Timestamp
		}
	}
	if res.Native != nil && (block == 0 || res.Native.AsOfBlock < block) {
		block, ts = res.Native.AsOfBlock, res.Native.Timestamp
	}
	return 0, block, ts
}

func dedupe(in []common.Address) []common.Address {
	seen := make(map[common.Address]bool, len(in))
	out := make([]common.Address, 0, len(in))
	for _, a := range in {
		if a == (common.Address{}) || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}
