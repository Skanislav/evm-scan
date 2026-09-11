package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
)

// LoadTokenLists compiles the configured token lists for one chain into a single
// filter, ready to be served and to annotate portfolio reads.
//
// Several lists are merged rather than kept apart because the question a caller
// asks is "has anyone vouched for this contract", not "which list was it on". The
// union is also what makes the filter small enough to be worth downloading: the
// lists overlap heavily on the tokens anybody holds.
//
// A list that fails to fetch is logged and skipped rather than fatal. These are
// third-party URLs on the public internet, and a deployment that refuses to start
// because someone's CDN is down has made an availability problem out of an
// optional hint.
func LoadTokenLists(ctx context.Context, chainID uint64, sources []string, log *slog.Logger) (*hintfilter.Cache, []common.Address, error) {
	if len(sources) == 0 {
		return nil, nil, nil
	}

	sub := hintfilter.Subkey(hintfilter.PublicSecret, chainID, hintfilter.KindToken)
	seen := map[common.Address]bool{}
	var keys []uint64
	var addrs []common.Address
	var names []string

	for _, src := range sources {
		tl, err := hintfilter.FetchTokenList(ctx, src)
		if err != nil {
			log.Warn("token list unavailable, skipping", "source", src, "chain_id", chainID, "err", err)
			continue
		}
		n := 0
		for _, a := range tl.Addresses(chainID) {
			if seen[a] {
				continue
			}
			seen[a] = true
			keys = append(keys, hintfilter.TokenKey(sub, a))
			addrs = append(addrs, a)
			n++
		}
		names = append(names, fmt.Sprintf("%s v%s (%d new)", tl.Name, tl.Version, n))
		log.Info("token list loaded", "source", src, "chain_id", chainID, "list", tl.Name, "new", n)
	}

	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("no token list for chain %d could be loaded", chainID)
	}

	f, err := hintfilter.Build(keys, hintfilter.Meta{
		ChainID: chainID, Kind: hintfilter.KindToken, EpochID: -1,
	})
	if err != nil {
		return nil, nil, err
	}
	enc, err := f.Encode()
	if err != nil {
		return nil, nil, err
	}

	source := fmt.Sprintf("%d list(s): %v", len(names), names)
	m := hintfilter.BuildManifest(f, enc, source, "")
	log.Info("token filter built", "chain_id", chainID, "tokens", f.Count(),
		"bytes", len(enc), "keccak256", m.Keccak256)
	// The addresses are kept alongside the filter because a filter can be tested
	// but never enumerated, and the private read path needs to iterate candidates
	// before it can test them. Without the list a reader would have to fetch it
	// from a third party, which reintroduces exactly the observer this is meant to
	// remove.
	return hintfilter.NewStatic(f, m, enc), addrs, nil
}

// knownToken reports whether a contract is on any token list this deployment
// loaded for the chain, and whether there was a list to ask at all.
//
// The second return matters. "Not on a list" and "there is no list" look identical
// to a caller that only gets a bool, and treating the second as the first would
// mark every token unknown on a deployment that configured none.
func (s *Server) knownToken(ctx context.Context, chainID uint64, token common.Address) (known, haveList bool) {
	c, ok := s.hints[tokenFilterName(chainID)]
	if !ok {
		return false, false
	}
	f, _, _, err := c.Get(ctx)
	if err != nil {
		return false, false
	}
	sub := hintfilter.Subkey(hintfilter.PublicSecret, chainID, hintfilter.KindToken)
	return f.Contains(hintfilter.TokenKey(sub, token)), true
}

func tokenFilterName(chainID uint64) string { return fmt.Sprintf("tokens-%d", chainID) }
func indexFilterName(chainID uint64) string { return fmt.Sprintf("index-%d", chainID) }
