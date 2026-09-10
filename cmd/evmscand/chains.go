package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/chainprofile"
	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/indexer"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Bringing a chain up.
//
// This lives here, rather than in internal/chainset, because it is the only place
// that knows how to build an indexer.Service — and internal/api reaches the
// indexer through an interface on purpose. The API asks for a chain by handing
// over a store.ChainProfile and calling a func; what that func does is this file's
// business.

// supervisor owns the running chains and knows how to start another.
type supervisor struct {
	st  *store.Store
	set *chainset.Set
	log *slog.Logger

	// runCtx bounds every indexer. cancel takes the whole process down, which is
	// the right answer for a chain the config named and the wrong one for a chain
	// somebody added over HTTP — see the note in launch.
	runCtx context.Context
	cancel context.CancelFunc
	wg     *sync.WaitGroup

	// mu serialises starts and stops so two requests for one chain cannot both
	// get past the "already running" check.
	mu sync.Mutex
}

// startConfigChain brings up a chain named in the YAML.
//
// Config is authoritative for these: its windows and its require_local_node are
// used as written, and a failure to start is fatal, because a deployment that
// cannot run the chain it was configured for cannot do its job.
func (sv *supervisor) startConfigChain(ctx context.Context, c config.Chain) error {
	opts := indexer.Options{
		ChainName:        c.Name,
		Confirmations:    c.Confirmations,
		BackfillWindow:   c.BackfillWindow,
		TailWindow:       c.TailWindow,
		PollInterval:     c.PollInterval.D(),
		BackfillInterval: c.BackfillInterval.D(),
		Discovery: indexer.DiscoveryOptions{
			Enabled:              c.Discovery.Enabled,
			Lookback:             c.Discovery.Lookback,
			MaxBlocksPerTick:     c.Discovery.MaxBlocksPerTick,
			Interval:             c.Discovery.Interval.D(),
			AutoPromote:          c.Discovery.AutoPromote,
			MinEvents:            c.Discovery.MinEvents,
			MinBlocks:            c.Discovery.MinBlocks,
			MaxPromotionsPerTick: c.Discovery.MaxPromotionsPerTick,
		},
	}

	entry, err := sv.dialAndBuild(ctx, c.ChainID, c.Name, c.Node, c.RequireLocal(), opts, c)
	if err != nil {
		return err
	}
	if err := sv.set.Add(entry); err != nil {
		entry.Source.Close()
		return err
	}

	// Record it, so /v1/chains can show a config chain next to an added one and
	// mean the same thing by both. Trust is derived from the endpoint: a node on
	// loopback, or a light client that checks logs against receipts roots, is
	// ours; anything else is somebody else's.
	prof := store.ChainProfile{
		ChainID: c.ChainID, Name: c.Name, Enabled: true,
		Source:  store.ChainSourceConfig,
		Trust:   trustForEndpoint(entry.Source.Endpoint()),
		NodeURL: c.Node,
		Tuning:  tuningJSON(opts),
	}
	p := chainprofile.For(c.ChainID, c.Name)
	prof.NativeSymbol, prof.NativeDecimals = p.NativeSymbol, p.NativeDecimals
	if p.WrappedNative != (common.Address{}) {
		wrapped := p.WrappedNative
		prof.WrappedNative = &wrapped
	}
	if err := sv.st.UpsertChainProfile(ctx, prof); err != nil {
		sv.log.Warn("could not record chain profile", "chain_id", c.ChainID, "err", err)
	}

	sv.launch(entry, true)
	return nil
}

// StartStored brings up a chain from its persisted profile. This is what
// POST /v1/chains calls, and what a restart calls for chains added that way.
func (sv *supervisor) StartStored(ctx context.Context, prof store.ChainProfile) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	if _, running := sv.set.Get(prof.ChainID); running {
		return fmt.Errorf("chain %d is already running", prof.ChainID)
	}
	if prof.NodeURL == "" {
		return fmt.Errorf("chain %d has no endpoint recorded", prof.ChainID)
	}

	p := chainprofile.For(prof.ChainID, prof.Name)
	t := parseTuning(prof.Tuning, p)
	opts := indexer.Options{
		ChainName:        prof.Name,
		Confirmations:    t.Confirmations,
		BackfillWindow:   t.BackfillWindow,
		TailWindow:       t.TailWindow,
		PollInterval:     parseDur(t.PollInterval, p.PollInterval),
		BackfillInterval: parseDur(t.BackfillInterval, p.BackfillInterval),
		Discovery: indexer.DiscoveryOptions{
			Enabled:          t.Discovery.Enabled,
			Lookback:         t.Discovery.Lookback,
			MaxBlocksPerTick: t.TailWindow,
			Interval:         parseDur(t.Discovery.Interval, p.PollInterval*3),
			// Auto-promotion is off for a chain added at runtime, deliberately.
			// Promotion spends a full backfill against a metered RPC, and an
			// operator who has just pointed us at a new network has not yet said
			// they want that bill.
			AutoPromote: false,
		},
	}

	// A chain added at runtime is reached over whatever endpoint the operator
	// gave, which is usually not loopback. require_local_node is therefore off
	// for it — and that is exactly why it lands as unverified.
	entry, err := sv.dialAndBuild(ctx, prof.ChainID, prof.Name, prof.NodeURL, false, opts,
		config.Chain{ChainID: prof.ChainID, Name: prof.Name})
	if err != nil {
		_ = sv.st.SetChainError(ctx, prof.ChainID, err)
		return err
	}
	if err := sv.set.Add(entry); err != nil {
		entry.Source.Close()
		return err
	}

	// Only now persist: a chain that could not be dialled is not one to remember
	// as running. Trust is re-derived from the endpoint rather than taken from
	// the request — a caller does not get to declare their RPC trustworthy, and
	// UpsertChainProfile will not overwrite a promotion an operator made anyway.
	prof.Enabled = true
	prof.Trust = trustForEndpoint(entry.Source.Endpoint())
	if err := sv.st.UpsertChainProfile(ctx, prof); err != nil {
		return err
	}
	_ = sv.st.SetChainError(ctx, prof.ChainID, nil)

	// Read the trust back rather than logging what we just tried to write. The
	// upsert deliberately does not touch it — an operator's promotion outlives a
	// restart — so `prof.Trust` here is the derived value, not the effective one,
	// and logging it would report a verified chain as unverified on every start.
	effectiveTrust := prof.Trust
	if stored, err := sv.st.GetChainProfile(ctx, prof.ChainID); err == nil {
		effectiveTrust = stored.Trust
	}

	sv.log.Info("chain started",
		"chain_id", prof.ChainID, "name", prof.Name,
		"node", entry.Source.Endpoint().Redacted(),
		"trust", effectiveTrust, "ens_name", prof.ENSName,
		"confirmations", opts.Confirmations, "discovery", opts.Discovery.Enabled)

	sv.launch(entry, false)
	return nil
}

// Stop takes a chain out of the set and closes its node. What it indexed stays.
func (sv *supervisor) Stop(_ context.Context, chainID uint64) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()

	entry, ok := sv.set.Remove(chainID)
	if !ok {
		return nil // already stopped; saying so twice is not an error
	}
	if entry.Source != nil {
		entry.Source.Close()
	}
	sv.log.Info("chain stopped", "chain_id", chainID)
	return nil
}

// dialAndBuild is the shared half: connect, check the node is on the network we
// think it is, and construct the indexer and pricer.
func (sv *supervisor) dialAndBuild(
	ctx context.Context,
	chainID uint64, name, node string, requireLocal bool,
	opts indexer.Options, pricingCfg config.Chain,
) (*chainset.Entry, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	src, err := chain.Dial(dialCtx, node, requireLocal)
	if err != nil {
		return nil, err
	}

	// The check the whole flow turns on. A node pointed at the wrong network
	// would silently produce a wrong index — an index keyed to a chain id whose
	// logs it never read. When the id came from ENS this is also what makes the
	// name meaningful: the registry says base is 8453, and the endpoint has to
	// agree before anything is written.
	actual, err := src.ChainID(dialCtx)
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("chain %d: read chain id: %w", chainID, err)
	}
	if actual != chainID {
		src.Close()
		return nil, fmt.Errorf("chain %d: that endpoint is chain %d", chainID, actual)
	}

	svc := indexer.New(src, sv.st, chainID, opts, sv.log)
	entry := &chainset.Entry{ID: chainID, Name: name, Source: src, Worker: svc}
	if p := newPricer(src, pricingCfg, sv.log); p != nil {
		entry.Pricer = p
	}
	return entry, nil
}

// launch runs a chain's indexer.
//
// fatal separates the two kinds of chain. A config chain that stops means the
// deployment cannot do what it was configured to do, so take the process down and
// let the supervisor restart it rather than serve stale answers behind a healthy
// API. A chain somebody added over HTTP is different: a bad endpoint there must
// not take the mainnet index with it, so it is recorded, dropped from the set,
// and left for the operator to fix.
func (sv *supervisor) launch(e *chainset.Entry, fatal bool) {
	svc, ok := e.Worker.(*indexer.Service)
	if !ok {
		return
	}
	sv.wg.Add(1)
	go func() {
		defer sv.wg.Done()
		err := svc.Run(sv.runCtx)
		if err == nil || sv.runCtx.Err() != nil {
			return
		}
		if fatal {
			sv.log.Error("indexer stopped", "chain_id", e.ID, "err", err)
			sv.cancel()
			return
		}
		sv.log.Error("indexer stopped; quarantining this chain",
			"chain_id", e.ID, "err", err,
			"note", "it was added at runtime, so the process stays up")
		if entry, ok := sv.set.Remove(e.ID); ok && entry.Source != nil {
			entry.Source.Close()
		}
		_ = sv.st.SetChainError(context.WithoutCancel(sv.runCtx), e.ID, err)
		_ = sv.st.SetChainTrust(context.WithoutCancel(sv.runCtx), e.ID, store.TrustQuarantined, "daemon")
	}()
}

// trustForEndpoint derives whether this chain's data may back a bonded
// commitment. Loopback is ours — either our own node, or a light client that
// verifies logs against receipts roots, which is the Helios posture. Anything
// else is a machine we do not run, and an operator has to say so on purpose.
func trustForEndpoint(ep chain.Endpoint) string {
	if ep.Local {
		return store.TrustVerified
	}
	return store.TrustUnverified
}

// --------------------------------------------------------------------------
// The tuning blob
// --------------------------------------------------------------------------

type chainTuning struct {
	Confirmations    uint64 `json:"confirmations"`
	BackfillWindow   uint64 `json:"backfill_window"`
	TailWindow       uint64 `json:"tail_window"`
	PollInterval     string `json:"poll_interval"`
	BackfillInterval string `json:"backfill_interval"`
	Discovery        struct {
		Enabled  bool   `json:"enabled"`
		Lookback uint64 `json:"lookback"`
		Interval string `json:"interval"`
	} `json:"discovery"`
}

func tuningJSON(o indexer.Options) json.RawMessage {
	var t chainTuning
	t.Confirmations = o.Confirmations
	t.BackfillWindow = o.BackfillWindow
	t.TailWindow = o.TailWindow
	t.PollInterval = o.PollInterval.String()
	t.BackfillInterval = o.BackfillInterval.String()
	t.Discovery.Enabled = o.Discovery.Enabled
	t.Discovery.Lookback = o.Discovery.Lookback
	t.Discovery.Interval = o.Discovery.Interval.String()
	b, err := json.Marshal(t)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// parseTuning reads the stored blob, filling anything missing or zero from the
// chain's built-in profile. A zero window is not a valid instruction — SweepLogs
// would silently substitute its own default, which is the "inherited the wrong
// chain's numbers" failure chainprofile exists to prevent.
func parseTuning(raw json.RawMessage, p chainprofile.Profile) chainTuning {
	var t chainTuning
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &t)
	}
	if t.BackfillWindow == 0 {
		t.BackfillWindow = p.BackfillWindow
	}
	if t.TailWindow == 0 {
		t.TailWindow = p.TailWindow
	}
	if t.Discovery.Lookback == 0 {
		t.Discovery.Lookback = p.TailWindow * 10
	}
	return t
}

func parseDur(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
