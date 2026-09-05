package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/token"
)

// Options tunes a chain's workers.
type Options struct {
	ChainName string
	// Confirmations is the depth at which a block is treated as final. Events above
	// this depth stay in the pending buffer, so a shallower reorg cannot corrupt the
	// rollup. Set 0 for a dev chain with instant finality.
	Confirmations uint64
	// BackfillWindow and TailWindow bound eth_getLogs ranges.
	BackfillWindow uint64
	TailWindow     uint64
	// PollInterval is the follower's floor cadence; a log subscription wakes it sooner.
	PollInterval     time.Duration
	BackfillInterval time.Duration

	// Discovery controls the head-watching sweep that finds contracts nobody has
	// registered yet.
	Discovery DiscoveryOptions
}

func (o Options) withDefaults() Options {
	if o.BackfillWindow == 0 {
		o.BackfillWindow = 5_000
	}
	if o.TailWindow == 0 {
		o.TailWindow = 1_000
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 3 * time.Second
	}
	if o.BackfillInterval <= 0 {
		o.BackfillInterval = time.Second
	}
	o.Discovery = o.Discovery.withDefaults()
	return o
}

// Service indexes one chain.
type Service struct {
	src     chain.Source
	st      *store.Store
	chainID uint64
	opt     Options
	log     *slog.Logger
	wake    chan struct{}

	// historyFloor is the oldest block this node can serve logs for. Backfills stop
	// here rather than at genesis, so the deployment never depends on a node that
	// kept all history.
	historyFloor atomic.Uint64
}

// New builds a Service for an already-dialled source.
func New(src chain.Source, st *store.Store, chainID uint64, opt Options, log *slog.Logger) *Service {
	return &Service{
		src:     src,
		st:      st,
		chainID: chainID,
		opt:     opt.withDefaults(),
		log:     log.With("chain_id", chainID),
		wake:    make(chan struct{}, 1),
	}
}

// Run drives the follower and backfiller until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.st.UpsertChain(ctx, s.chainID, s.opt.ChainName); err != nil {
		return err
	}
	if err := s.resolveHistoryFloor(ctx); err != nil {
		return err
	}

	go s.subscribe(ctx)
	go s.runBackfill(ctx)
	go s.runDiscovery(ctx)

	ticker := time.NewTicker(s.opt.PollInterval)
	defer ticker.Stop()

	for {
		if err := s.followTick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("follow tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// Nudge asks the follower to run a tick promptly, e.g. after a new registration.
func (s *Service) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// subscribe turns node-pushed logs into wake-ups.
//
// The subscription is deliberately only a latency hint: every log still reaches the
// index through the same eth_getLogs path the poller uses. Trusting pushed logs
// directly would mean two code paths that could disagree, and a dropped subscription
// would silently lose data.
func (s *Service) subscribe(ctx context.Context) {
	if !s.src.Endpoint().Streaming {
		s.log.Info("endpoint does not support subscriptions; polling only",
			"endpoint", s.src.Endpoint().String())
		return
	}

	backoff := time.Second
	for ctx.Err() == nil {
		ch := make(chan types.Log, 256)
		q := chain.Query{Topics: evmlog.WatchedTopics()}
		sub, err := s.src.SubscribeLogs(ctx, q, ch)
		if err != nil {
			if errors.Is(err, chain.ErrNotStreaming) {
				return
			}
			s.log.Warn("log subscription failed; will retry", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		s.log.Info("following head via eth_subscribe", "endpoint", s.src.Endpoint().String())
		backoff = time.Second

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				s.log.Warn("log subscription dropped; falling back to polling until it recovers", "err", err)
				sub.Unsubscribe()
				break drain
			case <-ch:
				s.Nudge()
			}
		}
	}
}

// --------------------------------------------------------------------------
// Follower
// --------------------------------------------------------------------------

func (s *Service) followTick(ctx context.Context) error {
	head, err := s.src.HeadBlock(ctx)
	if err != nil {
		return fmt.Errorf("head: %w", err)
	}

	if err := s.handleReorg(ctx); err != nil {
		return err
	}

	cursors, err := s.st.ListCursors(ctx, s.chainID)
	if err != nil {
		return err
	}
	if len(cursors) == 0 {
		return s.recordHead(ctx, head)
	}

	addrs := make([]common.Address, 0, len(cursors))
	tailOf := make(map[common.Address]uint64, len(cursors))
	from := ^uint64(0)
	for _, c := range cursors {
		addrs = append(addrs, c.Address)
		tailOf[c.Address] = c.TailBlock
		if c.TailBlock+1 < from {
			from = c.TailBlock + 1
		}
	}

	confirmedTo := uint64(0)
	if head > s.opt.Confirmations {
		confirmedTo = head - s.opt.Confirmations
	}

	if from > head {
		if _, err := s.st.PromotePending(ctx, s.chainID, confirmedTo); err != nil {
			return err
		}
		return s.recordHead(ctx, head)
	}

	seen := make(map[common.Address]uint64, len(cursors))
	q := chain.Query{From: from, To: head, Addresses: addrs, Topics: evmlog.WatchedTopics()}

	err = chain.SweepLogs(ctx, s.src, q, chain.ChunkOpts{Max: s.opt.TailWindow},
		func(ctx context.Context, cfrom, cto uint64, logs []types.Log) error {
			// An asset registered above `from` must not have its pre-anchor history
			// pulled in here; that range belongs to the backfiller, and letting both
			// claim it would double-count.
			kept := logs[:0:0]
			for i := range logs {
				if logs[i].BlockNumber > tailOf[logs[i].Address] {
					kept = append(kept, logs[i])
					seen[logs[i].Address]++
				}
			}

			if err := s.st.InsertPending(ctx, s.chainID, PendingFrom(kept)); err != nil {
				return err
			}
			if upto := min(cto, confirmedTo); upto >= cfrom {
				if _, err := s.st.PromotePending(ctx, s.chainID, upto); err != nil {
					return err
				}
			}
			return nil
		})
	if err != nil {
		return err
	}

	for _, c := range cursors {
		if err := s.st.AdvanceTail(ctx, s.chainID, c.Address, head, seen[c.Address]); err != nil {
			return err
		}
	}
	if _, err := s.st.PromotePending(ctx, s.chainID, confirmedTo); err != nil {
		return err
	}
	return s.recordHead(ctx, head)
}

// handleReorg compares buffered block hashes against the canonical chain and, on the
// first divergence, discards everything from that height so the follower rescans it.
//
// Only the pending buffer is ever affected: the rollup contains nothing above the
// confirmation depth, so there is nothing there to undo.
func (s *Service) handleReorg(ctx context.Context) error {
	hashes, err := s.st.PendingBlockHashes(ctx, s.chainID, 0)
	if err != nil || len(hashes) == 0 {
		return err
	}

	blocks := make([]uint64, 0, len(hashes))
	for b := range hashes {
		blocks = append(blocks, b)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })

	for _, b := range blocks {
		got, err := s.src.HeaderHash(ctx, b)
		if err != nil {
			// The height is gone from the canonical chain: treat as reorged out.
			return s.rewind(ctx, b, fmt.Sprintf("header %d unavailable: %v", b, err))
		}
		if got != hashes[b] {
			return s.rewind(ctx, b, fmt.Sprintf("hash mismatch at %d", b))
		}
	}
	return nil
}

func (s *Service) rewind(ctx context.Context, from uint64, reason string) error {
	dropped, err := s.st.DropPendingFrom(ctx, s.chainID, from)
	if err != nil {
		return err
	}
	to := uint64(0)
	if from > 0 {
		to = from - 1
	}
	if err := s.st.RewindTail(ctx, s.chainID, to); err != nil {
		return err
	}
	s.log.Warn("reorg detected; rescanning", "from_block", from, "dropped_pending", dropped, "reason", reason)
	return nil
}

func (s *Service) recordHead(ctx context.Context, head uint64) error {
	hash, err := s.src.HeaderHash(ctx, head)
	if err != nil {
		return nil // head moved under us; the next tick will catch it
	}
	return s.st.SetChainHead(ctx, s.chainID, head, hash)
}

// --------------------------------------------------------------------------
// Backfiller
// --------------------------------------------------------------------------

func (s *Service) runBackfill(ctx context.Context) {
	t := time.NewTicker(s.opt.BackfillInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.backfillTick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("backfill tick failed", "err", err)
		}
	}
}

// backfillTick advances every unfinished asset by one window.
//
// Working breadth-first across assets rather than finishing one at a time means a
// newly registered contract starts producing history immediately instead of queueing
// behind a large one.
func (s *Service) backfillTick(ctx context.Context) error {
	cursors, err := s.st.ListCursors(ctx, s.chainID)
	if err != nil {
		return err
	}

	for _, c := range cursors {
		if c.BackfillDone || ctx.Err() != nil {
			continue
		}
		if err := s.backfillAsset(ctx, c); err != nil {
			s.log.Error("backfill asset failed", "asset", c.Address.Hex(), "err", err)
		}
	}
	return ctx.Err()
}

func (s *Service) backfillAsset(ctx context.Context, c store.Cursor) error {
	asset, err := s.st.GetAsset(ctx, s.chainID, c.Address)
	if err != nil {
		return err
	}

	floor := resolveBackfillFloor(asset.HintFromBlock, s.HistoryFloor())
	if c.AnchorBlock == 0 || c.BackfillNext < floor {
		return s.finishBackfill(ctx, c.Address, floor)
	}
	if c.BackfillFloor != floor {
		if err := s.st.SetBackfillFloor(ctx, s.chainID, c.Address, floor); err != nil {
			return err
		}
	}

	to := c.BackfillNext
	from := floor
	if to >= floor+s.opt.BackfillWindow {
		from = to - s.opt.BackfillWindow + 1
	}

	if asset.Status == store.StatusPending {
		if err := s.st.SetAssetStatus(ctx, s.chainID, c.Address, store.StatusScanning); err != nil {
			return err
		}
	}

	logs, err := s.src.Logs(ctx, chain.Query{
		From:      from,
		To:        to,
		Addresses: []common.Address{c.Address},
		Topics:    evmlog.WatchedTopics(),
	})
	if err != nil {
		// A node that pruned further while we were running would fail here forever,
		// so re-probe the horizon before giving up on this window.
		if reprobed := s.reprobeHistoryFloor(ctx); reprobed > floor {
			s.log.Warn("node history horizon moved up; raising backfill floor",
				"asset", c.Address.Hex(), "old_floor", floor, "new_floor", reprobed)
			return nil
		}
		return fmt.Errorf("backfill getLogs [%d,%d]: %w", from, to, err)
	}

	// Blocks this deep are below the confirmation horizon, so they go straight into
	// the rollup without passing through the pending buffer.
	if err := s.st.FoldInteractions(ctx, s.chainID, Aggregate(logs)); err != nil {
		return err
	}

	// Classify and label the contract once, on the first backfill window.
	//
	// The registrant's declared kind is an assertion by an anonymous party, so
	// observed log shapes win when the two disagree: a hint cannot mislabel an asset
	// into a category it does not behave like.
	if c.AnchorBlock > 0 && c.BackfillNext+1 == c.AnchorBlock {
		std := asset.Standard
		if observed := evmlog.InferStandard(ObservedStandards(logs)); observed != evmlog.StandardUnknown {
			if std != uint8(observed) && std != uint8(evmlog.StandardUnknown) {
				s.log.Info("registrant's asset kind disagrees with observed events; trusting events",
					"asset", c.Address.Hex(), "declared", evmlog.Standard(std).String(), "observed", observed.String())
			}
			std = uint8(observed)
		}
		meta := token.Probe(ctx, s.src, c.Address)
		if err := s.st.SetAssetMetadata(ctx, s.chainID, c.Address,
			std, meta.Symbol, meta.Name, meta.Decimals); err != nil {
			return err
		}
	}

	done := from <= floor
	next := uint64(0)
	if from > 0 {
		next = from - 1
	}
	if err := s.st.AdvanceBackfill(ctx, s.chainID, c.Address, next, done, uint64(len(logs))); err != nil {
		return err
	}

	s.log.Debug("backfill window", "asset", c.Address.Hex(),
		"from", from, "to", to, "logs", len(logs), "done", done)

	if done {
		return s.finishBackfill(ctx, c.Address, floor)
	}
	return nil
}

func (s *Service) finishBackfill(ctx context.Context, addr common.Address, floor uint64) error {
	if err := s.st.AdvanceBackfill(ctx, s.chainID, addr, 0, true, 0); err != nil {
		return err
	}
	if err := s.st.SetBackfillFloor(ctx, s.chainID, addr, floor); err != nil {
		return err
	}
	s.log.Info("backfill complete", "asset", addr.Hex(), "floor", floor)
	return s.st.SetAssetStatus(ctx, s.chainID, addr, store.StatusLive)
}

// resolveBackfillFloor picks the block a history walk stops at.
//
// The requested from_block is what someone asked for; the node's history horizon is
// what is actually reachable. The stricter of the two wins, which is what keeps this
// design from depending on a node that retained history back to genesis — walking
// below the horizon would just fail on every tick, forever.
func resolveBackfillFloor(requested, historyFloor uint64) uint64 {
	if historyFloor > requested {
		return historyFloor
	}
	return requested
}

// HistoryFloor is the oldest block this chain's node can serve logs for.
func (s *Service) HistoryFloor() uint64 { return s.historyFloor.Load() }

// resolveHistoryFloor probes the node once at startup and caches the result.
func (s *Service) resolveHistoryFloor(ctx context.Context) error {
	head, err := s.src.HeadBlock(ctx)
	if err != nil {
		return fmt.Errorf("history floor: head: %w", err)
	}
	floor, err := chain.HistoryFloor(ctx, s.src, head)
	if err != nil {
		return fmt.Errorf("history floor: %w", err)
	}

	s.historyFloor.Store(floor)
	if err := s.st.SetHistoryFloor(ctx, s.chainID, floor); err != nil {
		return err
	}

	if floor == 0 {
		s.log.Info("node serves logs back to genesis", "head", head)
	} else {
		s.log.Info("node history horizon probed; backfills will stop here, not at genesis",
			"floor", floor, "head", head)
	}
	return nil
}

// reprobeHistoryFloor re-runs the probe after a backfill failure and returns the new
// floor. Errors are swallowed: the caller only wants to know whether the horizon moved.
func (s *Service) reprobeHistoryFloor(ctx context.Context) uint64 {
	if err := s.resolveHistoryFloor(ctx); err != nil {
		s.log.Debug("history floor re-probe failed", "err", err)
	}
	return s.HistoryFloor()
}
