package api

import (
	"net/http"

	"github.com/Skanislav/evm-scan/internal/indexer"
)

// The graph view answers a different question from the rest of the API: not "what
// has this account touched" but "who else touched the same things". The index
// already holds the answer — `interactions` is a membership table — so this is a
// read of what is there, with no node call and nothing derived.
//
// What it deliberately does not claim: an edge here is co-membership, not a
// transfer. The rollup folds events per (account, asset) and keeps no
// counterparties, so "A paid B" is not a fact this service has. Two accounts share
// an edge because both have events on the same contract, and the page says so.

type graphAssetJSON struct {
	Address  string `json:"address"`
	Standard string `json:"standard"`
	Symbol   string `json:"symbol,omitempty"`
	Name     string `json:"name,omitempty"`
	// Holders is how many accounts touched the contract; Members are the ones
	// actually returned, most active first. They differ when the width limit bit.
	Holders uint64            `json:"holders"`
	Members []graphMemberJSON `json:"members"`
}

type graphMemberJSON struct {
	Account    string   `json:"account"`
	FirstBlock uint64   `json:"first_block"`
	LastBlock  uint64   `json:"last_block"`
	EventCount uint64   `json:"event_count"`
	Roles      []string `json:"roles"`
}

// graph serves the membership lists the graph page draws from.
//
// It returns memberships rather than edges on purpose: k members imply k*(k-1)/2
// pairs, so the client expands what it needs and the wire carries the square root
// of it.
//
// Paged by `offset` over a stable busiest-first order, so a caller can draw a
// readable slice and then keep asking for more without the earlier pages moving
// under it.
func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	// A page, not a cap: `assets` is how many come back at once and `offset` is
	// where to resume. Widening `holders` grows the drawn edges quadratically,
	// which is why its ceiling is the tighter of the two.
	limit := intParam(r, "assets", 40, 1, 400)
	offset := intParam(r, "offset", 0, 0, 1_000_000)
	perAsset := intParam(r, "holders", 10, 2, 40)

	rows, total, err := s.d.Store.GraphMemberships(r.Context(), chainID, limit, offset, perAsset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	out := make([]graphAssetJSON, 0, len(rows))
	for _, m := range rows {
		g := graphAssetJSON{
			Address:  m.Asset.Hex(),
			Standard: standardName(m.Standard),
			Symbol:   m.Symbol,
			Name:     m.Name,
			Holders:  m.HolderTotal,
			Members:  make([]graphMemberJSON, 0, len(m.Holders)),
		}
		for _, h := range m.Holders {
			g.Members = append(g.Members, graphMemberJSON{
				Account:    h.Account.Hex(),
				FirstBlock: h.FirstBlock,
				LastBlock:  h.LastBlock,
				EventCount: h.EventCount,
				Roles:      indexer.RoleNames(h.Roles),
			})
		}
		out = append(out, g)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":     chainID,
		"offset":       offset,
		"limit":        limit,
		"holder_limit": perAsset,
		// Total is over assets that have any account at all, so it is the number a
		// caller would reach by paging to the end — not the size of the asset table.
		"total":        total,
		"has_more":     uint64(offset+len(out)) < total,
		"edge_meaning": "both accounts have events on this contract; not a transfer between them",
		"assets":       out,
	})
}
