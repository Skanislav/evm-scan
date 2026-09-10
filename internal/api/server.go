// Package api exposes the discovery index over HTTP.
//
// The contract with consumers is deliberately modest: this service answers "which
// contracts has this account touched, and what do we know about them". It is a hint,
// not an oracle. Balances are read live from our node at request time rather than
// derived from indexed events, and every response says which block it is as of, so a
// caller that needs certainty can verify against the chain itself.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/price"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Worker is the slice of a chain's indexer the HTTP layer needs.
//
// Kept as an interface so the API depends on behaviour rather than on the indexer
// package, and so promotion goes through the same path the discovery sweep uses.
type Worker interface {
	// Nudge asks the follower to run a tick promptly.
	Nudge()
	// Promote turns an observed contract into an indexed asset.
	Promote(ctx context.Context, addr common.Address, reason string) error
	// HistoryFloor is the oldest block this chain's node can serve logs for.
	HistoryFloor() uint64
	// DiscoveryThresholds are the activity levels at which a candidate qualifies
	// for promotion.
	DiscoveryThresholds() (minEvents, minBlocks uint64)
	// Health reports whether the worker is still doing its job. A worker that
	// exited, or that has been failing every tick, is unhealthy.
	Health() error
}

// Deps is everything the HTTP layer needs.
type Deps struct {
	Store   *store.Store
	Sources map[uint64]chain.Source
	Workers map[uint64]Worker
	// Pricers read on-chain price sources per chain. A chain without one simply
	// serves portfolios without values.
	Pricers map[uint64]*price.Pricer
	// ChainOrder is the configured order of Sources. Sources is a map, so ranging
	// it gives a different answer every start; anything that means "the chain this
	// deployment is mainly about" has to come from here. The first entry is what a
	// request without an explicit chain_id gets.
	ChainOrder        []uint64
	Registry          *hintreg.Client
	RegistryChainID   uint64
	Publisher         *hintreg.Publisher
	AllowRegistration bool
	// AuthToken, when set, is required as a bearer token on every endpoint that
	// spends something. Empty leaves those endpoints open.
	AuthToken string
	// Cost prices RPC traffic so /v1/status can report what the deployment spends.
	Cost       config.Cost
	CORSOrigin string
	WebDir     string
	Log        *slog.Logger
}

// Server routes and serves the API.
type Server struct {
	d   Deps
	mux *http.ServeMux
	// started is the epoch for RPC counters, which are per-process.
	started time.Time
	chains  []uint64
	// meta caches token symbol/name/decimals by chain and address. A deployed
	// contract's metadata does not change, and reading it costs a verified eth_call
	// on a light client, so it is worth never asking twice.
	meta tokenMetaCache
}

// New builds the router.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux(), started: time.Now()}
	for _, id := range d.ChainOrder {
		if _, ok := d.Sources[id]; ok {
			s.chains = append(s.chains, id)
		}
	}
	for id := range d.Sources {
		if slices.Contains(s.chains, id) {
			continue
		}
		s.chains = append(s.chains, id)
	}

	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/status", s.status)
	s.mux.HandleFunc("GET /v1/assets", s.listAssets)
	s.mux.HandleFunc("POST /v1/assets", s.registerAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}", s.getAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}/accounts", s.assetAccounts)
	s.mux.HandleFunc("GET /v1/accounts", s.listAccounts)
	s.mux.HandleFunc("GET /v1/accounts/{address}", s.accountAssets)
	s.mux.HandleFunc("GET /v1/accounts/{address}/contracts", s.accountContracts)
	s.mux.HandleFunc("GET /v1/accounts/{address}/portfolio", s.accountPortfolio)
	s.mux.HandleFunc("GET /v1/prices", s.listPrices)
	s.mux.HandleFunc("GET /v1/lens", s.listLenses)
	s.mux.HandleFunc("GET /v1/candidates", s.listCandidates)
	s.mux.HandleFunc("POST /v1/candidates/{address}/promote", s.promoteCandidate)
	s.mux.HandleFunc("POST /v1/candidates/{address}/spam", s.markCandidateSpam)
	s.mux.HandleFunc("POST /v1/candidates/{address}/unspam", s.clearCandidateSpam)
	s.mux.HandleFunc("GET /v1/decisions", s.listDecisions)
	s.mux.HandleFunc("GET /v1/epochs", s.listEpochs)
	s.mux.HandleFunc("POST /v1/epochs", s.createEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}", s.getEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}/proof", s.epochProof)
	s.mux.HandleFunc("GET /v1/epochs/{id}/snapshot", s.epochSnapshot)
	// ERC-3668 gateway for HintRegistry.contractsOf.
	s.mux.HandleFunc("GET /ccip/{sender}/{data}", s.ccipGet)
	s.mux.HandleFunc("POST /ccip", s.ccipPost)

	if d.WebDir != "" {
		s.mux.Handle("/", http.FileServer(http.Dir(d.WebDir)))
	}
	return s
}

func (s *Server) Handler() http.Handler { return s.withMiddleware(s.mux) }

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := s.d.CORSOrigin; o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Headers", "content-type, authorization")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "this endpoint requires a token",
				errors.New("send it as Authorization: Bearer <token>; it is EVMSCAN_API_TOKEN on the deployment"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// spendsSomething reports whether a request would cost the deployment money or
// quota: publishing an epoch is the publisher's gas, promoting or registering an asset
// is a backfill against a paid RPC, and a verdict writes an operator's judgement into
// the index. Reads are never guarded — the whole point of the index is that anyone can
// query it.
//
// The rule is "every POST, minus an allowlist" rather than a list of guarded paths,
// because the two fail in opposite directions: a forgotten entry here leaves a new
// mutation open, while a forgotten exception only makes one too strict. The single
// exception is the ERC-3668 callback, which a resolver anywhere on the internet has to
// be able to reach.
func guarded(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path != "/ccip"
}

// authorized checks the bearer token on the endpoints that mutate. With no token
// configured everything stays open, which is what a laptop and the demo want.
func (s *Server) authorized(r *http.Request) bool {
	if s.d.AuthToken == "" || !guarded(r) {
		return true
	}
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	// Constant time, so a token cannot be recovered by measuring the comparison.
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), []byte(s.d.AuthToken)) == 1
}

// --------------------------------------------------------------------------
// Request helpers
// --------------------------------------------------------------------------

type apiError struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string, detail error) {
	e := apiError{Error: msg}
	if detail != nil {
		e.Detail = detail.Error()
	}
	writeJSON(w, code, e)
}

// chainOf resolves the chain_id parameter, defaulting to the only configured chain.
func (s *Server) chainOf(r *http.Request) (uint64, chain.Source, error) {
	raw := r.URL.Query().Get("chain_id")
	if raw == "" {
		// A second chain is usually there to host the registry, not because the
		// deployment is equally about both, so default to the first configured one
		// rather than making every caller name it. An explicit chain_id still wins.
		if len(s.chains) > 0 {
			id := s.chains[0]
			return id, s.d.Sources[id], nil
		}
		return 0, nil, fmt.Errorf("no chains are configured")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("chain_id %q is not a number", raw)
	}
	src, ok := s.d.Sources[id]
	if !ok {
		return 0, nil, fmt.Errorf("chain_id %d is not indexed by this deployment", id)
	}
	return id, src, nil
}

func parseAddress(raw string) (common.Address, error) {
	raw = strings.TrimSpace(raw)
	if !common.IsHexAddress(raw) {
		return common.Address{}, fmt.Errorf("%q is not a 20-byte hex address", raw)
	}
	return common.HexToAddress(raw), nil
}

func standardName(s uint8) string { return evmlog.Standard(s).String() }

// --------------------------------------------------------------------------
// Health and status
// --------------------------------------------------------------------------

// health is what a supervisor should probe. It fails when the database is
// unreachable, a node stops answering, or an indexer has died or is failing every
// tick; a process in any of those states should be restarted, not left serving
// stale answers.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks := map[string]string{}
	healthy := true
	fail := func(name string, err error) {
		checks[name] = err.Error()
		healthy = false
	}

	if err := s.d.Store.Ping(ctx); err != nil {
		fail("database", err)
	} else {
		checks["database"] = "ok"
	}
	for _, id := range s.chains {
		name := fmt.Sprintf("chain_%d", id)
		if _, err := s.d.Sources[id].HeadBlock(ctx); err != nil {
			fail(name+"_node", err)
		} else {
			checks[name+"_node"] = "ok"
		}
		if wk, ok := s.d.Workers[id]; ok {
			if err := wk.Health(); err != nil {
				fail(name+"_indexer", err)
			} else {
				checks[name+"_indexer"] = "ok"
			}
		}
	}

	code, status := http.StatusOK, "ok"
	if !healthy {
		code, status = http.StatusServiceUnavailable, "degraded"
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

type chainStatus struct {
	ChainID uint64 `json:"chain_id"`
	// Name is the operator's label for the chain, so a UI can say "chain 1 · mainnet"
	// without carrying its own table of well-known ids.
	Name          string `json:"name,omitempty"`
	Node          string `json:"node"`
	NodeTransport string `json:"node_transport"`
	NodeLocal     bool   `json:"node_local"`
	Head          uint64 `json:"head_block"`
	// HistoryFloor is the oldest block this node can serve logs for. Backfills stop
	// here, so it bounds how complete any account's history can be.
	HistoryFloor    uint64 `json:"history_floor"`
	DiscoveryCursor uint64 `json:"discovery_cursor"`
	Assets          int64  `json:"assets"`
	Accounts        int64  `json:"accounts"`
	Interactions    int64  `json:"interactions"`
	PendingEvents   int64  `json:"pending_events"`
	BackfillDone    int    `json:"assets_backfilled"`
	Candidates      int64  `json:"candidates_observed"`
	CandidatesReady int64  `json:"candidates_promotable"`
	CandidatesSpam  int64  `json:"candidates_spam"`
	// Pricing says where this chain's prices come from, or that they do not.
	Pricing *pricingStatus `json:"pricing,omitempty"`
	// RPC is what this chain has asked its endpoint for, and what that costs.
	RPC   *rpcStatus `json:"rpc,omitempty"`
	Error string     `json:"error,omitempty"`
}

// rpcStatus reports RPC usage for one chain since this process started.
//
// BilledDirectly is the field to read first. When it is false the endpoint is a
// local light client, and the provider is billed for what that client fetches
// upstream to verify each answer — several requests per call, none of them visible
// here. Calls is then a lower bound on the bill, not the bill.
type rpcStatus struct {
	Calls          map[string]uint64 `json:"calls"`
	Total          uint64            `json:"total"`
	BilledDirectly bool              `json:"billed_directly"`
	Since          string            `json:"since"`
	// Per-hour rates, which is what a monthly bill is actually made of.
	CallsPerHour float64  `json:"calls_per_hour"`
	ComputeUnits *float64 `json:"compute_units,omitempty"`
	USD          *float64 `json:"estimated_usd,omitempty"`
	USDPerDay    *float64 `json:"estimated_usd_per_day,omitempty"`
	Note         string   `json:"note,omitempty"`
}

// rpcUsage turns a chain's call counters into a bill.
//
// The rate is per request because that is how the metering measured out: on dRPC,
// 32,780 CU over 1,639 requests and 40,780 over 2,039 are both exactly 20 CU per
// call, so nothing here needs a per-method table.
func (s *Server) rpcUsage(src chain.Source) *rpcStatus {
	m, ok := src.(chain.MeteredSource)
	if !ok {
		return nil
	}
	calls, total := m.RPCCalls()
	uptime := time.Since(s.started)
	hours := uptime.Hours()

	out := &rpcStatus{
		Calls:          calls,
		Total:          total,
		BilledDirectly: m.BilledDirectly(),
		Since:          uptime.Round(time.Second).String(),
	}
	if hours > 0 {
		out.CallsPerHour = float64(total) / hours
	}
	if !out.BilledDirectly {
		out.Note = "endpoint is a local light client; the provider is billed for its " +
			"upstream fetches, which are several per call and not counted here. Treat " +
			"these numbers as a lower bound."
	}

	if rate := s.d.Cost.Rate(); rate > 0 {
		cu := float64(total) * s.d.Cost.CUPerRequest
		usd := float64(total) * rate
		out.ComputeUnits = &cu
		out.USD = &usd
		if hours > 0 {
			perDay := usd / hours * 24
			out.USDPerDay = &perDay
		}
	}
	return out
}

type pricingStatus struct {
	Enabled      bool   `json:"enabled"`
	FeedRegistry bool   `json:"feed_registry"`
	NativeFeed   bool   `json:"native_usd_feed"`
	PinnedFeeds  int    `json:"pinned_feeds"`
	UniswapV3    bool   `json:"uniswap_v3"`
	UniswapV2    bool   `json:"uniswap_v2"`
	QuoteTokens  int    `json:"quote_tokens"`
	TWAPWindow   string `json:"twap_window"`
	// NativeUSD is the native asset's current USD price, when the feed answered.
	NativeUSD string `json:"native_usd,omitempty"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := struct {
		Chains   []chainStatus `json:"chains"`
		Registry *struct {
			ChainID uint64 `json:"chain_id"`
			Address string `json:"address"`
			// RewardPerBlockWei is what the registry pays a publisher per newly covered
			// block of a funded asset; MinFundingWei is the least a request deposits.
			RewardPerBlockWei string `json:"reward_per_block_wei,omitempty"`
			MinFundingWei     string `json:"min_funding_wei,omitempty"`
			// AssetBondWei is what registerAsset locks for an asset nobody has
			// registered yet. requestIndexing wants assetBond + minFunding for a new
			// asset and minFunding for one that already exists, so a caller building
			// that transaction needs both numbers.
			AssetBondWei string `json:"asset_bond_wei,omitempty"`
		} `json:"registry,omitempty"`
		Publisher string `json:"publisher,omitempty"`
	}{}

	for _, id := range s.chains {
		src := s.d.Sources[id]
		ep := src.Endpoint()
		cs := chainStatus{
			ChainID:       id,
			Node:          ep.Redacted(),
			NodeTransport: string(ep.Transport),
			NodeLocal:     ep.Local,
		}
		if head, err := src.HeadBlock(ctx); err == nil {
			cs.Head = head
		} else {
			cs.Error = err.Error()
		}
		if st, err := s.d.Store.Stats(ctx, id); err == nil {
			cs.Assets, cs.Accounts = st.Assets, st.Accounts
			cs.Interactions, cs.PendingEvents = st.Interactions, st.Pending
		}
		if cursors, err := s.d.Store.ListCursors(ctx, id); err == nil {
			for _, c := range cursors {
				if c.BackfillDone {
					cs.BackfillDone++
				}
			}
		}
		if cur, err := s.d.Store.DiscoveryCursor(ctx, id); err == nil {
			cs.DiscoveryCursor = cur
		}
		if name, err := s.d.Store.ChainName(ctx, id); err == nil {
			cs.Name = name
		}
		if w, ok := s.d.Workers[id]; ok {
			cs.HistoryFloor = w.HistoryFloor()
			minEvents, minBlocks := w.DiscoveryThresholds()
			if cst, err := s.d.Store.CandidateStats(ctx, id, minEvents, minBlocks); err == nil {
				cs.Candidates, cs.CandidatesReady = cst.Observed, cst.Promotable
				cs.CandidatesSpam = cst.Spam
			}
		}
		cs.RPC = s.rpcUsage(src)
		cs.Pricing = &pricingStatus{}
		if p := s.pricerFor(id); p != nil {
			src := p.Sources()
			cs.Pricing = &pricingStatus{
				Enabled:      true,
				FeedRegistry: src.FeedRegistry != (common.Address{}),
				NativeFeed:   src.NativeUSDFeed != (common.Address{}) || src.FeedRegistry != (common.Address{}),
				PinnedFeeds:  len(src.Feeds),
				UniswapV3:    src.V3Factory != (common.Address{}),
				UniswapV2:    src.V2Factory != (common.Address{}),
				QuoteTokens:  len(src.QuoteTokens),
				TWAPWindow:   p.TWAPWindow().String(),
			}
			if res, _ := s.quotes(ctx, p, nil); res != nil && res.Native != nil {
				cs.Pricing.NativeUSD = price.FormatPrice(res.Native.USD)
			}
		}
		out.Chains = append(out.Chains, cs)
	}

	if s.d.Registry != nil {
		out.Registry = &struct {
			ChainID           uint64 `json:"chain_id"`
			Address           string `json:"address"`
			RewardPerBlockWei string `json:"reward_per_block_wei,omitempty"`
			MinFundingWei     string `json:"min_funding_wei,omitempty"`
			AssetBondWei      string `json:"asset_bond_wei,omitempty"`
		}{ChainID: s.d.RegistryChainID, Address: s.d.Registry.Address().Hex()}
		if v, err := s.d.Registry.RewardPerBlock(ctx); err == nil {
			out.Registry.RewardPerBlockWei = v.String()
		}
		if v, err := s.d.Registry.MinFunding(ctx); err == nil {
			out.Registry.MinFundingWei = v.String()
		}
		if v, err := s.d.Registry.AssetBond(ctx); err == nil {
			out.Registry.AssetBondWei = v.String()
		}
	}
	if s.d.Publisher != nil {
		out.Publisher = s.d.Publisher.Address().Hex()
	}
	writeJSON(w, http.StatusOK, out)
}
