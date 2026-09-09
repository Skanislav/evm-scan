package api

import (
	"context"
	"sync"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/lens"
)

// tokenMeta is the part of a token's identity that never changes once it is
// deployed, which is why it can be cached without an expiry.
type tokenMeta struct {
	Symbol   string
	Name     string
	Decimals *int16
}

// tokenMetaCache remembers metadata per chain and address. Reading it costs a
// verified eth_call against a light client — about a second per batch of twenty —
// so a listing that asked every time took a minute to render.
type tokenMetaCache struct {
	mu sync.RWMutex
	m  map[uint64]map[common.Address]tokenMeta
}

func (c *tokenMetaCache) get(chainID uint64, addr common.Address) (tokenMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[chainID][addr]
	return v, ok
}

func (c *tokenMetaCache) put(chainID uint64, addr common.Address, v tokenMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[uint64]map[common.Address]tokenMeta{}
	}
	if c.m[chainID] == nil {
		c.m[chainID] = map[common.Address]tokenMeta{}
	}
	c.m[chainID][addr] = v
}

// lookup fills what it can from the cache and reads the rest through the deployless
// lens, one eth_call per batch and the batches concurrently. Failures are silent:
// this decorates a listing, and an unnamed row still carries the address that
// identifies it.
func (c *tokenMetaCache) lookup(
	ctx context.Context, src chain.Source, chainID uint64, addrs []common.Address,
) map[common.Address]tokenMeta {
	const (
		batch    = 20
		parallel = 4
	)

	out := make(map[common.Address]tokenMeta, len(addrs))
	var missing []common.Address
	seen := make(map[common.Address]bool, len(addrs))
	for _, a := range addrs {
		if seen[a] {
			continue
		}
		seen[a] = true
		if v, ok := c.get(chainID, a); ok {
			out[a] = v
			continue
		}
		missing = append(missing, a)
	}
	if len(missing) == 0 {
		return out
	}

	var chunks [][]common.Address
	for i := 0; i < len(missing); i += batch {
		chunks = append(chunks, missing[i:min(i+batch, len(missing))])
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, parallel)
	)
	for _, chunk := range chunks {
		wg.Add(1)
		go func(chunk []common.Address) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			q := make([]lens.TokenQuery, len(chunk))
			for i, a := range chunk {
				q[i] = lens.TokenQuery{Token: a}
			}
			res, err := lens.Query(ctx, src, lens.Request{Tokens: q, SkipNonce: true})
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, t := range res.Tokens {
				v := tokenMeta{Symbol: t.Symbol, Name: t.Name, Decimals: t.Decimals}
				out[t.Address] = v
				c.put(chainID, t.Address, v)
			}
		}(chunk)
	}
	wg.Wait()
	return out
}
