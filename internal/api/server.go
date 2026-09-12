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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/chainset"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/price"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Worker is the slice of a chain's indexer the HTTP layer needs.
//
// Kept as an interface so the API depends on behaviour rather than on the indexer
// package, and so promotion goes through the same path the discovery sweep uses.
// It is defined next to the chain set, which speaks in the same terms.
type Worker = chainset.Worker

// Deps is everything the HTTP layer needs.
type Deps struct {
	Store *store.Store
	// Chains is the live set of chains this process runs — their nodes, indexers
	// and pricers. It is read through rather than copied, because it changes while
	// the server is up: a chain added over the API has to be visible to the next
	// request, not to the next restart. Its order is the configured order, and its
	// first entry is what a request without an explicit chain_id gets.
	Chains *chainset.Set
	// StartChain brings a chain up: dial, verify the node's own chain id against
	// the one asked for, persist, start indexing. Supplied by cmd/evmscand,
	// because that is the only place that knows how to build an indexer — putting
	// it behind a func is what keeps this package's dependency on the indexer down
	// to the Worker interface. Nil means this deployment cannot add chains.
	StartChain func(ctx context.Context, p store.ChainProfile) error
	// StopChain stops one and closes its node.
	StopChain func(ctx context.Context, chainID uint64) error
	// ENS resolves chain names through the on.eth registry. Nil where the
	// deployment has no Ethereum mainnet endpoint to ask.
	ENS *ens.Resolver
	// ENSParent is the name a HintResolver serves the index under (docs/ENS.md).
	// Set, it lets account responses carry the account's hint name; the daemon
	// never resolves anything through it.
	ENSParent         string
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
	// TokenFilters are the compiled token lists, keyed by chain id. Built by
	// LoadTokenLists before the server starts, because fetching third-party URLs is
	// startup work and does not belong on a request path.
	TokenFilters map[uint64]*hintfilter.Cache
	// TokenAddresses is the enumerable form of the same lists, keyed by chain id.
	TokenAddresses map[uint64][]common.Address
}

// Server routes and serves the API.
type Server struct {
	d   Deps
	mux *http.ServeMux
	// started is the epoch for RPC counters, which are per-process.
	started time.Time
	// meta caches token symbol/name/decimals by chain and address. A deployed
	// contract's metadata does not change, and reading it costs a verified eth_call
	// on a light client, so it is worth never asking twice.
	meta tokenMetaCache
	// hints are the published membership filters, keyed by the name in their URL.
	// A fixed set of names, so the name in a request can never become a path.
	//
	// Token filters are put here at startup, because they come from lists fetched
	// once. Index filters are made on first ask instead: chains come and go at
	// runtime now, and anything built eagerly per chain would miss one added later
	// and strand one removed.
	hintsMu   sync.Mutex
	hints     map[string]*hintfilter.Cache
	hintLists map[string][]common.Address
	// funding is the registry's per-asset funding, read in the background so the
	// asset list never waits on a verified eth_call per row. See funding.go.
	funding *fundingCache
	// registry is the deployment's adjudication rules, read once: every field in
	// it is an immutable of the contract, and /v1/status is polled every few
	// seconds by every open page. See registry_status.go.
	regMu sync.Mutex
	reg   *registryRules
}

// indexFilter returns the index filter cache for a chain, making it on first ask.
func (s *Server) indexFilter(chainID uint64) *hintfilter.Cache {
	name := indexFilterName(chainID)
	s.hintsMu.Lock()
	defer s.hintsMu.Unlock()
	if c, ok := s.hints[name]; ok {
		return c
	}
	if s.d.Store == nil {
		return nil
	}
	c := hintfilter.NewCache(chainID,
		func(ctx context.Context, toBlock uint64) ([]hintfilter.AccountAssetSet, error) {
			sets, err := s.d.Store.SnapshotIndex(ctx, chainID, toBlock)
			if err != nil {
				return nil, err
			}
			out := make([]hintfilter.AccountAssetSet, len(sets))
			for i, v := range sets {
				out[i] = hintfilter.AccountAssetSet{Account: v.Account, Assets: v.Assets}
			}
			return out, nil
		},
		func(ctx context.Context) (uint64, uint64, error) {
			return s.d.Store.CoverageRange(ctx, chainID)
		})
	s.hints[name] = c
	return c
}

// lookupHint finds a filter by the name in its URL, making an index filter for a
// chain this deployment runs if it has not been asked for yet. A name that is not
// one of ours resolves to nothing — never to a path.
func (s *Server) lookupHint(name string) (*hintfilter.Cache, bool) {
	s.hintsMu.Lock()
	c, ok := s.hints[name]
	s.hintsMu.Unlock()
	if ok {
		return c, true
	}
	for _, e := range s.d.Chains.Entries() {
		if name == indexFilterName(e.ID) {
			if c := s.indexFilter(e.ID); c != nil {
				return c, true
			}
		}
	}
	return nil, false
}

// New builds the router.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux(), started: time.Now(), funding: newFundingCache()}
	s.hints = map[string]*hintfilter.Cache{}
	s.hintLists = map[string][]common.Address{}
	for chainID, c := range d.TokenFilters {
		if c != nil {
			s.hints[tokenFilterName(chainID)] = c
		}
	}
	for chainID, addrs := range d.TokenAddresses {
		if len(addrs) > 0 {
			s.hintLists[tokenFilterName(chainID)] = addrs
		}
	}
	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/status", s.status)
	s.mux.HandleFunc("GET /v1/chains", s.listChains)
	s.mux.HandleFunc("GET /v1/chains/resolve", s.resolveChain)
	s.mux.HandleFunc("POST /v1/chains", s.addChain)
	s.mux.HandleFunc("PATCH /v1/chains/{id}", s.patchChain)
	s.mux.HandleFunc("DELETE /v1/chains/{id}", s.deleteChain)
	s.mux.HandleFunc("GET /v1/assets", s.listAssets)
	s.mux.HandleFunc("POST /v1/assets", s.registerAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}", s.getAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}/accounts", s.assetAccounts)
	s.mux.HandleFunc("GET /v1/accounts", s.listAccounts)
	s.mux.HandleFunc("GET /v1/accounts/{address}", s.accountAssets)
	s.mux.HandleFunc("GET /v1/accounts/{address}/contracts", s.accountContracts)
	s.mux.HandleFunc("GET /v1/accounts/{address}/portfolio", s.accountPortfolio)
	s.mux.HandleFunc("GET /v1/graph", s.graph)
	s.mux.HandleFunc("GET /v1/prices", s.listPrices)
	s.mux.HandleFunc("GET /v1/lens", s.listLenses)
	s.mux.HandleFunc("GET /v1/hints", s.listHints)
	s.mux.HandleFunc("GET /v1/hints/{file}", s.serveHintFilter)
	s.mux.HandleFunc("GET /v1/candidates", s.listCandidates)
	s.mux.HandleFunc("POST /v1/candidates/{address}/promote", s.promoteCandidate)
	s.mux.HandleFunc("POST /v1/candidates/{address}/spam", s.markCandidateSpam)
	s.mux.HandleFunc("POST /v1/candidates/{address}/unspam", s.clearCandidateSpam)
	// Against an asset, not a candidate: a contract someone paid to register never
	// passes through discovery, so no candidate verdict can ever reach it.
	s.mux.HandleFunc("POST /v1/assets/{address}/report", s.reportAsset)
	s.mux.HandleFunc("POST /v1/assets/{address}/unreport", s.clearAssetReport)
	s.mux.HandleFunc("GET /v1/decisions", s.listDecisions)
	s.mux.HandleFunc("GET /v1/epochs", s.listEpochs)
	s.mux.HandleFunc("POST /v1/epochs", s.createEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}", s.getEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}/proof", s.epochProof)
	s.mux.HandleFunc("GET /v1/epochs/{id}/snapshot", s.epochSnapshot)
	s.mux.HandleFunc("GET /v1/epochs/{id}/manifest", s.epochManifest)
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
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
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

// guarded reports whether a request has to carry the operator's token.
//
// Everything that is not a read is guarded, because each of them costs the deployment
// something it cannot get back: publishing an epoch is the publisher's gas, promoting
// or registering an asset is a backfill against a paid RPC, and a verdict writes an
// operator's judgement into the index. Adding a chain is the largest by some way — an
// epoch or a promotion is one bounded spend, a chain is a per-block RPC bill from then
// on — and a PATCH that sets trust to "verified" is larger still, because it puts the
// publisher's bond behind logs served by a node we do not run.
//
// The rule is "every mutating method, minus an allowlist" rather than a list of guarded
// paths. The two fail in opposite directions: a path forgotten from a guard list leaves
// a new mutation open, while a route forgotten from an exception list only makes one too
// strict, and someone notices immediately. The single exception is the ERC-3668
// callback, which a resolver anywhere on the internet has to be able to reach.
//
// Reads are never guarded — the whole point of the index is that anyone can query it.
func guarded(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
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
		e, ok := s.d.Chains.First()
		if !ok {
			return 0, nil, fmt.Errorf("no chains are configured")
		}
		return e.ID, e.Source, nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("chain_id %q is not a number", raw)
	}
	src, ok := s.d.Chains.Source(id)
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
	for _, e := range s.d.Chains.Entries() {
		id := e.ID
		name := fmt.Sprintf("chain_%d", id)
		if _, err := e.Source.HeadBlock(ctx); err != nil {
			fail(name+"_node", err)
		} else {
			checks[name+"_node"] = "ok"
		}
		if wk, ok := s.d.Chains.Worker(id); ok {
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
		Chains    []chainStatus `json:"chains"`
		Registry  *registryJSON `json:"registry,omitempty"`
		Publisher string        `json:"publisher,omitempty"`
	}{}

	for _, e := range s.d.Chains.Entries() {
		id, src := e.ID, e.Source
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
		if w, ok := s.d.Chains.Worker(id); ok {
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

	out.Registry = s.registryStatus(ctx)
	if s.d.Publisher != nil {
		out.Publisher = s.d.Publisher.Address().Hex()
	}
	writeJSON(w, http.StatusOK, out)
}
