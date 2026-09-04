// Package api exposes the discovery index over HTTP.
//
// The contract with consumers is deliberately modest: this service answers "which
// contracts has this account touched, and what do we know about them". It is a hint,
// not an oracle. Balances are read live from our node at request time rather than
// derived from indexed events, and every response says which block it is as of, so a
// caller that needs certainty can verify against the chain itself.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/store"
)

// Nudger wakes a chain's follower.
type Nudger interface{ Nudge() }

// Deps is everything the HTTP layer needs.
type Deps struct {
	Store             *store.Store
	Sources           map[uint64]chain.Source
	Nudgers           map[uint64]Nudger
	Registry          *hintreg.Client
	RegistryChainID   uint64
	Publisher         *hintreg.Publisher
	AllowRegistration bool
	CORSOrigin        string
	WebDir            string
	Log               *slog.Logger
}

// Server routes and serves the API.
type Server struct {
	d      Deps
	mux    *http.ServeMux
	chains []uint64
}

// New builds the router.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux()}
	for id := range d.Sources {
		s.chains = append(s.chains, id)
	}

	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/status", s.status)
	s.mux.HandleFunc("GET /v1/assets", s.listAssets)
	s.mux.HandleFunc("POST /v1/assets", s.registerAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}", s.getAsset)
	s.mux.HandleFunc("GET /v1/assets/{address}/accounts", s.assetAccounts)
	s.mux.HandleFunc("GET /v1/accounts/{address}", s.accountAssets)
	s.mux.HandleFunc("GET /v1/accounts/{address}/contracts", s.accountContracts)
	s.mux.HandleFunc("GET /v1/epochs", s.listEpochs)
	s.mux.HandleFunc("POST /v1/epochs", s.createEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}", s.getEpoch)
	s.mux.HandleFunc("GET /v1/epochs/{id}/proof", s.epochProof)

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
			w.Header().Set("Access-Control-Allow-Headers", "content-type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		if len(s.chains) == 1 {
			id := s.chains[0]
			return id, s.d.Sources[id], nil
		}
		return 0, nil, fmt.Errorf("chain_id is required when multiple chains are indexed")
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

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

type chainStatus struct {
	ChainID       uint64 `json:"chain_id"`
	Node          string `json:"node"`
	NodeTransport string `json:"node_transport"`
	NodeLocal     bool   `json:"node_local"`
	Head          uint64 `json:"head_block"`
	Assets        int64  `json:"assets"`
	Accounts      int64  `json:"accounts"`
	Interactions  int64  `json:"interactions"`
	PendingEvents int64  `json:"pending_events"`
	BackfillDone  int    `json:"assets_backfilled"`
	Error         string `json:"error,omitempty"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := struct {
		Chains   []chainStatus `json:"chains"`
		Registry *struct {
			ChainID uint64 `json:"chain_id"`
			Address string `json:"address"`
		} `json:"registry,omitempty"`
		Publisher string `json:"publisher,omitempty"`
	}{}

	for _, id := range s.chains {
		src := s.d.Sources[id]
		ep := src.Endpoint()
		cs := chainStatus{
			ChainID:       id,
			Node:          ep.Raw,
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
		out.Chains = append(out.Chains, cs)
	}

	if s.d.Registry != nil {
		out.Registry = &struct {
			ChainID uint64 `json:"chain_id"`
			Address string `json:"address"`
		}{ChainID: s.d.RegistryChainID, Address: s.d.Registry.Address().Hex()}
	}
	if s.d.Publisher != nil {
		out.Publisher = s.d.Publisher.Address().Hex()
	}
	writeJSON(w, http.StatusOK, out)
}
