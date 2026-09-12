package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// Demand: a reader's vote for what to index next.
//
// The lookup page shows an account's holdings, and the ones the index does not keep
// can be voted for from there. A vote is a priority signal and nothing else: it
// orders the promotion queue and, with min_voters set, lets a contract enough
// accounts asked for be promoted without waiting on activity. It never changes what
// a balance read says and a spam verdict still outranks it.
//
// What the daemon learns is the account and the contracts it voted for, which is
// what a hosted lookup already tells it. The voter is stored blinded under a
// per-deployment salt (store.RecordDemand), so the table counts an account once
// and cannot be walked back to who holds what.

type demandRequest struct {
	ChainID uint64   `json:"chain_id"`
	Account string   `json:"account"`
	Assets  []string `json:"assets"`
}

type demandJSON struct {
	Address       string `json:"address"`
	Voters        uint64 `json:"voters"`
	OnchainVoters uint64 `json:"onchain_voters"`
	LastAt        string `json:"last_at"`
	Indexed       bool   `json:"indexed"`
	Candidate     bool   `json:"candidate"`
	Spam          bool   `json:"spam"`
	EventCount    uint64 `json:"event_count"`
	// Promotable says whether the vote count alone clears min_voters; false when
	// demand promotion is off for the chain.
	Promotable bool `json:"promotable"`
}

func (s *Server) recordDemand(w http.ResponseWriter, r *http.Request) {
	var req demandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body", err)
		return
	}
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	if req.ChainID != 0 {
		chainID = req.ChainID
	}
	if _, ok := s.d.Chains.Worker(chainID); !ok {
		writeErr(w, http.StatusBadRequest, "unknown chain", nil)
		return
	}
	if !common.IsHexAddress(req.Account) {
		writeErr(w, http.StatusBadRequest, "account must be an address", nil)
		return
	}
	if len(req.Assets) == 0 {
		writeErr(w, http.StatusBadRequest, "assets is empty", nil)
		return
	}
	if len(req.Assets) > store.MaxDemandPerVote {
		writeErr(w, http.StatusBadRequest, "too many assets in one vote", nil)
		return
	}
	addrs := make([]common.Address, 0, len(req.Assets))
	for _, a := range req.Assets {
		if !common.IsHexAddress(a) {
			writeErr(w, http.StatusBadRequest, "assets must be addresses", nil)
			return
		}
		addrs = append(addrs, common.HexToAddress(a))
	}

	ctx := r.Context()
	added, err := s.d.Store.RecordDemand(ctx, chainID, common.HexToAddress(req.Account), addrs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record", err)
		return
	}
	totals, err := s.d.Store.DemandFor(ctx, chainID, addrs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	voters := make(map[string]uint64, len(totals))
	for a, n := range totals {
		voters[a.Hex()] = n
	}
	var minVoters uint64
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		_, _, minVoters = wk.DiscoveryThresholds()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":   chainID,
		"recorded":   added,
		"voters":     voters,
		"min_voters": minVoters,
	})
}

func (s *Server) listDemand(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	rows, err := s.d.Store.ListDemand(r.Context(), chainID, intParam(r, "limit", 50, 1, 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}
	var minVoters uint64
	if wk, ok := s.d.Chains.Worker(chainID); ok {
		_, _, minVoters = wk.DiscoveryThresholds()
	}
	out := make([]demandJSON, len(rows))
	for i, d := range rows {
		out[i] = demandJSON{
			Address: d.Address.Hex(), Voters: d.Voters, OnchainVoters: d.OnchainVoters,
			LastAt:  d.LastAt.UTC().Format(time.RFC3339),
			Indexed: d.Indexed, Candidate: d.Candidate, Spam: d.Spam, EventCount: d.EventCount,
			Promotable: minVoters > 0 && d.Voters >= minVoters && !d.Indexed && !d.Spam,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":   chainID,
		"min_voters": minVoters,
		"demand":     out,
	})
}
