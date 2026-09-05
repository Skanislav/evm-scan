package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/token"
)

// DiscoveryOptions controls the head-watching sweep that finds contracts.
type DiscoveryOptions struct {
	Enabled bool
	// Lookback is how far behind the head a cold start begins. Discovery is a
	// forward-looking process: it deliberately does not walk history, because the
	// point is to learn what is active now and only then decide what deserves a
	// backfill.
	Lookback uint64
	// MaxBlocksPerTick bounds catch-up work so discovery cannot starve the workers
	// that serve the actual index.
	MaxBlocksPerTick uint64
	Interval         time.Duration

	// AutoPromote turns activity thresholds into indexed assets without a human or an
	// on-chain registration. Off by default: the registry is the authoritative signal
	// for "this contract is worth indexing", and promotion commits real work.
	AutoPromote          bool
	MinEvents            uint64
	MinBlocks            uint64
	MaxPromotionsPerTick int
}

func (o DiscoveryOptions) withDefaults() DiscoveryOptions {
	if o.Lookback == 0 {
		o.Lookback = 10_000
	}
	if o.MaxBlocksPerTick == 0 {
		o.MaxBlocksPerTick = 5_000
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.MinEvents == 0 {
		o.MinEvents = 100
	}
	if o.MinBlocks == 0 {
		o.MinBlocks = 25
	}
	if o.MaxPromotionsPerTick == 0 {
		o.MaxPromotionsPerTick = 5
	}
	return o
}

// CandidatesFrom folds logs into per-contract observation counters.
//
// Only counters are produced — no per-account rows. That asymmetry is the whole point:
// watching every contract on the chain has to stay cheap, and only a promoted contract
// earns the expensive per-account index.
func CandidatesFrom(logs []types.Log, skip map[common.Address]bool) []store.Candidate {
	type acc struct {
		c      store.Candidate
		blocks map[uint64]struct{}
		seen   map[evmlog.Standard]int
	}
	byAddr := make(map[common.Address]*acc)

	for i := range logs {
		l := &logs[i]
		if l.Removed || skip[l.Address] {
			continue
		}
		d, ok := evmlog.Decode(l)
		if !ok {
			continue
		}

		a := byAddr[l.Address]
		if a == nil {
			a = &acc{
				c: store.Candidate{
					Address:        l.Address,
					FirstSeenBlock: l.BlockNumber,
					LastSeenBlock:  l.BlockNumber,
				},
				blocks: map[uint64]struct{}{},
				seen:   map[evmlog.Standard]int{},
			}
			byAddr[l.Address] = a
		}
		if l.BlockNumber < a.c.FirstSeenBlock {
			a.c.FirstSeenBlock = l.BlockNumber
		}
		if l.BlockNumber > a.c.LastSeenBlock {
			a.c.LastSeenBlock = l.BlockNumber
		}
		a.c.EventCount++
		a.blocks[l.BlockNumber] = struct{}{}
		if d.Standard != evmlog.StandardUnknown {
			a.seen[d.Standard]++
		}
	}

	out := make([]store.Candidate, 0, len(byAddr))
	for _, a := range byAddr {
		a.c.BlocksSeen = uint64(len(a.blocks))
		a.c.Standard = uint8(evmlog.InferStandard(a.seen))
		out = append(out, a.c)
	}
	return out
}

// runDiscovery watches the head for contracts we are not yet indexing.
func (s *Service) runDiscovery(ctx context.Context) {
	if !s.opt.Discovery.Enabled {
		s.log.Info("runtime discovery disabled; only registered assets will be indexed")
		return
	}

	t := time.NewTicker(s.opt.Discovery.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.discoveryTick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("discovery tick failed", "err", err)
		}
	}
}

func (s *Service) discoveryTick(ctx context.Context) error {
	head, err := s.src.HeadBlock(ctx)
	if err != nil {
		return err
	}

	cursor, err := s.st.DiscoveryCursor(ctx, s.chainID)
	if err != nil {
		return err
	}

	from := cursor + 1
	if cursor == 0 {
		// Cold start near the head rather than at genesis: history is the expensive
		// thing, and we have not yet decided any of it is worth reading.
		from = 0
		if head > s.opt.Discovery.Lookback {
			from = head - s.opt.Discovery.Lookback
		}
	}
	if floor := s.HistoryFloor(); from < floor {
		from = floor
	}
	if from > head {
		return s.autoPromote(ctx)
	}

	to := head
	if span := s.opt.Discovery.MaxBlocksPerTick; to-from+1 > span {
		to = from + span - 1
	}

	// Everything we already index is excluded: the tail scanner covers those, and
	// counting them again would just be noise in the ranking.
	known, err := s.indexedAddresses(ctx)
	if err != nil {
		return err
	}

	// No address filter — this is the one query that deliberately looks at the whole
	// chain. The topic filter still keeps the node from shipping logs we would drop.
	q := chain.Query{From: from, To: to, Topics: evmlog.WatchedTopics()}
	err = chain.SweepLogs(ctx, s.src, q, chain.ChunkOpts{Max: s.opt.TailWindow},
		func(ctx context.Context, cfrom, cto uint64, logs []types.Log) error {
			return s.st.UpsertCandidates(ctx, s.chainID, CandidatesFrom(logs, known))
		})
	if err != nil {
		return err
	}

	// Advance only after the whole range folded in: candidate counters accumulate, so
	// a partial range that got replayed would double-count.
	if err := s.st.SetDiscoveryCursor(ctx, s.chainID, to); err != nil {
		return err
	}
	s.log.Debug("discovery sweep", "from", from, "to", to, "head", head)

	return s.autoPromote(ctx)
}

func (s *Service) indexedAddresses(ctx context.Context) (map[common.Address]bool, error) {
	cursors, err := s.st.ListCursors(ctx, s.chainID)
	if err != nil {
		return nil, err
	}
	out := make(map[common.Address]bool, len(cursors))
	for _, c := range cursors {
		out[c.Address] = true
	}
	return out, nil
}

func (s *Service) autoPromote(ctx context.Context) error {
	d := s.opt.Discovery
	if !d.AutoPromote {
		return nil
	}

	cands, err := s.st.PromotableCandidates(ctx, s.chainID, d.MinEvents, d.MinBlocks, d.MaxPromotionsPerTick)
	if err != nil {
		return err
	}
	for _, c := range cands {
		reason := fmt.Sprintf("auto: %d events across %d blocks", c.EventCount, c.BlocksSeen)
		if err := s.Promote(ctx, c.Address, reason); err != nil {
			s.log.Error("promotion failed", "asset", c.Address.Hex(), "err", err)
		}
	}
	return nil
}

// Promote turns an observed contract into an indexed asset.
//
// This is the moment the design commits real work: from here the contract gets a
// per-account index and a backfill down to whatever history the node retains. Nothing
// before this point costs more than a counter.
func (s *Service) Promote(ctx context.Context, addr common.Address, reason string) error {
	head, err := s.src.HeadBlock(ctx)
	if err != nil {
		return err
	}

	code, err := s.src.CodeAt(ctx, addr)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		return fmt.Errorf("indexer: %s has no contract code", addr.Hex())
	}

	var standard uint8
	if c, err := s.st.GetCandidate(ctx, s.chainID, addr); err == nil {
		standard = c.Standard
	}
	meta := token.Probe(ctx, s.src, addr)

	// The backfill floor is the node's history horizon, not genesis. Asking for more
	// than the node holds would just fail repeatedly at the bottom of the walk.
	floor := s.HistoryFloor()

	if _, err := s.st.RegisterAsset(ctx, store.Asset{
		ChainID:       s.chainID,
		Address:       addr,
		Standard:      standard,
		Symbol:        meta.Symbol,
		Name:          meta.Name,
		Decimals:      meta.Decimals,
		HintFromBlock: floor,
		Source:        store.SourceDiscovered,
		Promoted:      true,
	}, head); err != nil {
		return err
	}
	if err := s.st.MarkCandidatePromoted(ctx, s.chainID, addr, reason); err != nil {
		return err
	}

	s.log.Info("promoted contract to indexed asset",
		"asset", addr.Hex(), "symbol", meta.Symbol,
		"anchor_block", head, "backfill_floor", floor, "reason", reason)
	s.Nudge()
	return nil
}

// DiscoveryThresholds reports the activity levels at which a candidate qualifies for
// promotion, so callers can show why something is or is not eligible.
func (s *Service) DiscoveryThresholds() (minEvents, minBlocks uint64) {
	return s.opt.Discovery.MinEvents, s.opt.Discovery.MinBlocks
}
