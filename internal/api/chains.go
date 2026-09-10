package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/chainprofile"
	"github.com/Skanislav/evm-scan/internal/ens"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Chain management over HTTP.
//
// The point of these endpoints is that adding a network stops being a file edit
// and a restart. Two things make that safe rather than merely convenient:
//
//   - The chain id is never taken on trust. It comes from ENS or from the
//     operator, and either way the dialled node is asked for its own and refused
//     if the two disagree. cmd/evmscand has always done this for config chains;
//     here it finally has a second opinion to check against.
//   - A chain added this way is `unverified`, so nothing it produces can back a
//     bonded commitment until somebody says otherwise on purpose.

type chainJSON struct {
	ChainID uint64 `json:"chain_id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
	// Trust says whether this chain's data may back an on-chain commitment.
	// Verified is a claim about who served the logs, nothing else.
	Trust string `json:"trust"`
	// Verified is Trust == "verified", repeated as a boolean because it is the
	// flag a consumer filters on and a string comparison is easy to get wrong.
	Verified bool `json:"verified"`
	// Node is redacted. The URL usually carries a provider's API key.
	Node          string `json:"node,omitempty"`
	NodeTransport string `json:"node_transport,omitempty"`
	NodeLocal     bool   `json:"node_local"`

	Head         uint64 `json:"head_block,omitempty"`
	HistoryFloor uint64 `json:"history_floor,omitempty"`

	Native *nativeJSON `json:"native,omitempty"`

	ENSName    string     `json:"ens_name,omitempty"`
	TrustSetBy string     `json:"trust_set_by,omitempty"`
	TrustSetAt *time.Time `json:"trust_set_at,omitempty"`

	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

type nativeJSON struct {
	Symbol   string `json:"symbol"`
	Decimals int16  `json:"decimals"`
	Wrapped  string `json:"wrapped,omitempty"`
}

// listChains reports every chain this deployment knows about: the ones running
// now, and the ones recorded but stopped.
func (s *Server) listChains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Stored rows first, so a chain that is recorded but not running still shows
	// up with the reason it is not.
	byID := map[uint64]*chainJSON{}
	var order []uint64
	if s.d.Store != nil {
		if rows, err := s.d.Store.ListChainProfiles(ctx, false); err == nil {
			for _, p := range rows {
				byID[p.ChainID] = chainViewFromProfile(p)
				order = append(order, p.ChainID)
			}
		}
	}

	// Then overlay what is actually up. The set is the truth about "running";
	// the table is the truth about "configured".
	for _, e := range s.d.Chains.Entries() {
		v, ok := byID[e.ID]
		if !ok {
			v = &chainJSON{ChainID: e.ID, Name: e.Name, Enabled: true,
				Source: store.ChainSourceConfig, Trust: store.TrustUnverified}
			byID[e.ID] = v
			order = append(order, e.ID)
		}
		v.Running = true
		if v.Name == "" {
			v.Name = e.Name
		}
		ep := e.Source.Endpoint()
		v.Node, v.NodeTransport, v.NodeLocal = ep.Redacted(), string(ep.Transport), ep.Local
		if head, err := e.Source.HeadBlock(ctx); err == nil {
			v.Head = head
		}
		if wk, ok := s.d.Chains.Worker(e.ID); ok {
			v.HistoryFloor = wk.HistoryFloor()
		}
		if v.Native == nil {
			p := chainprofile.For(e.ID, e.Name)
			v.Native = &nativeJSON{Symbol: p.NativeSymbol, Decimals: p.NativeDecimals}
			if p.WrappedNative != (common.Address{}) {
				v.Native.Wrapped = p.WrappedNative.Hex()
			}
		}
	}

	out := make([]*chainJSON, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chains": out,
		// Which chain a request without an explicit chain_id means.
		"default_chain_id": defaultChainID(s),
		"can_add":          s.d.StartChain != nil,
		"ens_available":    s.d.ENS != nil,
	})
}

func defaultChainID(s *Server) uint64 {
	if e, ok := s.d.Chains.First(); ok {
		return e.ID
	}
	return 0
}

func chainViewFromProfile(p store.ChainProfile) *chainJSON {
	v := &chainJSON{
		ChainID: p.ChainID, Name: p.Name, Enabled: p.Enabled,
		Source: p.Source, Trust: p.Trust, Verified: p.Trust == store.TrustVerified,
		ENSName: p.ENSName, TrustSetBy: p.TrustSetBy, TrustSetAt: p.TrustSetAt,
		LastError: p.LastError, LastErrorAt: p.LastErrorAt,
	}
	if p.NativeSymbol != "" {
		v.Native = &nativeJSON{Symbol: p.NativeSymbol, Decimals: p.NativeDecimals}
		if p.WrappedNative != nil {
			v.Native.Wrapped = p.WrappedNative.Hex()
		}
	}
	return v
}

// --------------------------------------------------------------------------
// Resolving a name
// --------------------------------------------------------------------------

type resolveJSON struct {
	Label   string `json:"label"`
	ENSName string `json:"ens_name"`
	ChainID uint64 `json:"chain_id"`
	URL     string `json:"url,omitempty"`
	Avatar  string `json:"avatar,omitempty"`
	// Resolver is echoed so a caller can check which contract answered against
	// the one they trust, the same way price responses echo their sources.
	Resolver string `json:"resolver"`
	// KnownProfile says whether there is built-in tuning for this chain. False
	// means the windows and confirmations will be conservative guesses.
	KnownProfile bool `json:"known_profile"`
	// Suggested is what the daemon would use, so the UI can show it before the
	// operator commits and let them argue with it.
	Suggested      *suggestedJSON `json:"suggested,omitempty"`
	AlreadyRunning bool           `json:"already_running"`
}

type suggestedJSON struct {
	Name           string      `json:"name"`
	Confirmations  uint64      `json:"confirmations"`
	BackfillWindow uint64      `json:"backfill_window"`
	TailWindow     uint64      `json:"tail_window"`
	BlockTime      string      `json:"block_time"`
	Native         *nativeJSON `json:"native"`
}

// resolveChain turns a name into a chain id, through ENS's on.eth registry.
//
// A read, so it is not behind the token: it costs one eth_call and reports what
// anyone could read off mainnet themselves.
func (s *Server) resolveChain(w http.ResponseWriter, r *http.Request) {
	if s.d.ENS == nil {
		writeErr(w, http.StatusNotImplemented, "name resolution is not available here",
			errors.New("it needs an Ethereum mainnet endpoint: index mainnet, or set ens.node"))
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("name"))
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "name is required",
			errors.New(`try ?name=base, which resolves base.on.eth`))
		return
	}

	c, err := s.d.ENS.Lookup(r.Context(), raw)
	if errors.Is(err, ens.ErrNotRegistered) {
		// Not a fault. Most chains are not in the registry, and the caller's next
		// move is to enter the details by hand rather than to give up.
		name, _ := ens.Qualify(raw)
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":    "no chain is registered under that name",
			"detail":   name + " resolves to nothing; enter the chain id and RPC directly",
			"label":    strings.TrimSuffix(name, "."+ens.Parent),
			"ens_name": name,
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not resolve the name", err)
		return
	}

	out := resolveJSON{
		Label: c.Label, ENSName: c.Name, ChainID: c.ChainID,
		URL: c.URL, Avatar: c.Avatar, Resolver: c.Resolver.Hex(),
	}
	_, out.KnownProfile = chainprofile.Lookup(c.ChainID)
	_, out.AlreadyRunning = s.d.Chains.Get(c.ChainID)

	p := chainprofile.For(c.ChainID, c.Label)
	out.Suggested = &suggestedJSON{
		Name: p.Name, Confirmations: p.Confirmations,
		BackfillWindow: p.BackfillWindow, TailWindow: p.TailWindow,
		BlockTime: p.BlockTime.String(),
		Native:    &nativeJSON{Symbol: p.NativeSymbol, Decimals: p.NativeDecimals},
	}
	if p.WrappedNative != (common.Address{}) {
		out.Suggested.Native.Wrapped = p.WrappedNative.Hex()
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Adding, retuning and removing
// --------------------------------------------------------------------------

type addChainRequest struct {
	// One of ENSName or ChainID identifies the network. Both is allowed and they
	// must agree; neither is the error.
	ENSName string `json:"ens_name"`
	ChainID uint64 `json:"chain_id"`
	Name    string `json:"name"`
	Node    string `json:"node"`

	Native *struct {
		Symbol   string `json:"symbol"`
		Decimals int16  `json:"decimals"`
		Wrapped  string `json:"wrapped"`
	} `json:"native"`

	Confirmations  *uint64 `json:"confirmations"`
	BackfillWindow *uint64 `json:"backfill_window"`
	TailWindow     *uint64 `json:"tail_window"`
	Discovery      *struct {
		Enabled  *bool   `json:"enabled"`
		Lookback *uint64 `json:"lookback"`
	} `json:"discovery"`
}

// tuning is the JSON blob stored in chains.profile, and what cmd/evmscand reads
// back to build an indexer. Kept here rather than in store because store's job is
// to keep it, not to understand it.
type tuning struct {
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

func (s *Server) addChain(w http.ResponseWriter, r *http.Request) {
	if s.d.StartChain == nil {
		writeErr(w, http.StatusNotImplemented, "this deployment cannot add chains at runtime", nil)
		return
	}

	var req addChainRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body", err)
		return
	}
	if strings.TrimSpace(req.Node) == "" {
		writeErr(w, http.StatusBadRequest, "node is required",
			errors.New("the registry names chains, it does not publish endpoints; supply your own RPC"))
		return
	}

	ctx := r.Context()

	// Establish the chain id. A name resolves to one; an operator can state one;
	// if both are given they have to agree, because a disagreement means one of
	// them is about a different network.
	chainID, ensName := req.ChainID, ""
	if req.ENSName != "" {
		if s.d.ENS == nil {
			writeErr(w, http.StatusNotImplemented, "name resolution is not available here",
				errors.New("supply chain_id instead"))
			return
		}
		c, err := s.d.ENS.Lookup(ctx, req.ENSName)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not resolve that name", err)
			return
		}
		if chainID != 0 && chainID != c.ChainID {
			writeErr(w, http.StatusBadRequest, "the name and the chain id disagree",
				fmt.Errorf("%s is chain %d, not %d", c.Name, c.ChainID, chainID))
			return
		}
		chainID, ensName = c.ChainID, c.Name
		if req.Name == "" {
			req.Name = c.Label
		}
	}
	if chainID == 0 {
		writeErr(w, http.StatusBadRequest, "no chain identified",
			errors.New("give ens_name (say \"base\") or chain_id"))
		return
	}
	if _, running := s.d.Chains.Get(chainID); running {
		writeErr(w, http.StatusConflict, "that chain is already running", nil)
		return
	}

	p := chainprofile.For(chainID, req.Name)
	prof := store.ChainProfile{
		ChainID: chainID,
		Name:    firstNonEmpty(req.Name, p.Name),
		Enabled: true,
		Source:  store.ChainSourceAPI,
		// Derived, never asserted. A remote RPC is unverified however the chain
		// was identified — resolving a name says nothing about who serves the
		// logs. Raising it is a separate, deliberate PATCH.
		Trust:          store.TrustUnverified,
		NodeURL:        strings.TrimSpace(req.Node),
		NativeSymbol:   p.NativeSymbol,
		NativeDecimals: p.NativeDecimals,
		ENSName:        ensName,
	}
	if ensName != "" {
		now := time.Now().UTC()
		prof.ENSResolved = &now
	}
	if p.WrappedNative != (common.Address{}) {
		wrapped := p.WrappedNative
		prof.WrappedNative = &wrapped
	}
	if n := req.Native; n != nil {
		if n.Symbol != "" {
			prof.NativeSymbol = n.Symbol
		}
		if n.Decimals > 0 {
			prof.NativeDecimals = n.Decimals
		}
		if n.Wrapped != "" {
			if !common.IsHexAddress(n.Wrapped) {
				writeErr(w, http.StatusBadRequest, "native.wrapped is not an address", nil)
				return
			}
			a := common.HexToAddress(n.Wrapped)
			prof.WrappedNative = &a
		}
	}
	if ep, err := chain.ParseEndpoint(prof.NodeURL); err == nil && ep.Local {
		// A node on loopback is ours by definition, which is the whole premise of
		// require_local_node.
		prof.Trust = store.TrustVerified
	}

	t := tuning{
		Confirmations:    p.Confirmations,
		BackfillWindow:   p.BackfillWindow,
		TailWindow:       p.TailWindow,
		PollInterval:     p.PollInterval.String(),
		BackfillInterval: p.BackfillInterval.String(),
	}
	// Discovery is off unless asked for, and that default is worth explaining.
	// The sweep queries eth_getLogs with no address filter, which is the single
	// most restricted call on a shared endpoint — free tiers routinely answer it
	// with "specify an address" or demand an archive plan. A chain added at
	// runtime is reached over exactly that kind of endpoint, so switching
	// discovery on by default means a failing tick every few seconds on a network
	// the operator has only just pointed at. They can turn it on once they know
	// what their provider allows.
	t.Discovery.Enabled = false
	t.Discovery.Lookback = p.TailWindow * 10
	t.Discovery.Interval = (p.PollInterval * 3).String()
	if req.Confirmations != nil {
		t.Confirmations = *req.Confirmations
	}
	if req.BackfillWindow != nil && *req.BackfillWindow > 0 {
		t.BackfillWindow = *req.BackfillWindow
	}
	if req.TailWindow != nil && *req.TailWindow > 0 {
		t.TailWindow = *req.TailWindow
	}
	if d := req.Discovery; d != nil {
		if d.Enabled != nil {
			t.Discovery.Enabled = *d.Enabled
		}
		if d.Lookback != nil {
			t.Discovery.Lookback = *d.Lookback
		}
	}
	blob, err := json.Marshal(t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not encode the tuning", err)
		return
	}
	prof.Tuning = blob

	// StartChain dials, asks the node its chain id, refuses a mismatch, persists
	// and starts. The refusal is the point of the whole flow.
	if err := s.d.StartChain(ctx, prof); err != nil {
		writeErr(w, http.StatusBadRequest, "could not start that chain", err)
		return
	}

	out, err := s.d.Store.GetChainProfile(ctx, chainID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "started, but could not read it back", err)
		return
	}
	v := chainViewFromProfile(out)
	v.Running = true
	if e, ok := s.d.Chains.Get(chainID); ok {
		ep := e.Source.Endpoint()
		v.Node, v.NodeTransport, v.NodeLocal = ep.Redacted(), string(ep.Transport), ep.Local
	}
	writeJSON(w, http.StatusCreated, map[string]any{"chain": v})
}

type patchChainRequest struct {
	Enabled *bool `json:"enabled"`
	// Trust promotes or demotes a chain. Promoting is the largest thing this API
	// can be asked to do; see the note on the handler.
	Trust string `json:"trust"`
	// TrustSetBy labels who decided. Free text — an audit note, not an identity
	// the system can verify.
	TrustSetBy string `json:"trust_set_by"`
}

// patchChain changes a chain's trust or whether it is enabled.
//
// Setting trust to "verified" says: this chain's logs may sit behind the
// publisher's bond. On a remote RPC that is a statement about a machine we do not
// run, and it is worth more than any other single call here — publishing an epoch
// spends gas, promoting an asset spends quota, and both are recoverable. So it is
// gated by EVMSCAN_API_TOKEN like the rest of the spending endpoints, recorded
// with who and when, and logged at warn level rather than info.
func (s *Server) patchChain(w http.ResponseWriter, r *http.Request) {
	chainID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chain id is not a number", err)
		return
	}
	var req patchChainRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<15)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body", err)
		return
	}
	ctx := r.Context()

	if req.Trust != "" {
		switch req.Trust {
		case store.TrustVerified, store.TrustUnverified:
		default:
			writeErr(w, http.StatusBadRequest, "trust must be verified or unverified",
				errors.New("quarantined is set by the daemon, not by hand"))
			return
		}
		if err := s.d.Store.SetChainTrust(ctx, chainID, req.Trust, req.TrustSetBy); err != nil {
			writeErr(w, http.StatusBadRequest, "could not set trust", err)
			return
		}
		if req.Trust == store.TrustVerified {
			node := ""
			if e, ok := s.d.Chains.Get(chainID); ok {
				node = e.Source.Endpoint().Redacted()
			}
			s.log().Warn("chain promoted to verified: its data may now back a bonded commitment",
				"chain_id", chainID, "node", node, "by", req.TrustSetBy)
		} else {
			s.log().Info("chain demoted to unverified", "chain_id", chainID, "by", req.TrustSetBy)
		}
	}

	if req.Enabled != nil {
		if err := s.d.Store.SetChainEnabled(ctx, chainID, *req.Enabled); err != nil {
			writeErr(w, http.StatusBadRequest, "could not change enabled", err)
			return
		}
		if !*req.Enabled && s.d.StopChain != nil {
			if err := s.d.StopChain(ctx, chainID); err != nil {
				writeErr(w, http.StatusInternalServerError, "disabled, but could not stop it", err)
				return
			}
		}
	}

	out, err := s.d.Store.GetChainProfile(ctx, chainID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "unknown chain", err)
		return
	}
	v := chainViewFromProfile(out)
	if e, ok := s.d.Chains.Get(chainID); ok {
		v.Running = true
		ep := e.Source.Endpoint()
		v.Node, v.NodeTransport, v.NodeLocal = ep.Redacted(), string(ep.Transport), ep.Local
	}
	writeJSON(w, http.StatusOK, map[string]any{"chain": v})
}

// deleteChain stops a chain and disables it. What it indexed is kept: the data is
// still true, and re-enabling should not mean re-walking history.
func (s *Server) deleteChain(w http.ResponseWriter, r *http.Request) {
	chainID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "chain id is not a number", err)
		return
	}
	if s.d.StopChain == nil {
		writeErr(w, http.StatusNotImplemented, "this deployment cannot stop chains at runtime", nil)
		return
	}
	ctx := r.Context()
	if err := s.d.StopChain(ctx, chainID); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not stop that chain", err)
		return
	}
	if err := s.d.Store.SetChainEnabled(ctx, chainID, false); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "stopped, but could not record it", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stopped": chainID,
		"note":    "the index this chain built is kept; re-enable it to resume rather than rescan",
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// log is never nil, so handlers can record a decision without a guard.
func (s *Server) log() *slog.Logger {
	if s.d.Log != nil {
		return s.d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
