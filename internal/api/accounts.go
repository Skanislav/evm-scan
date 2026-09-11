package api

import (
	"errors"
	"net/http"

	"github.com/Skanislav/evm-scan/internal/store"
)

type accountRankJSON struct {
	Account    string `json:"account"`
	AssetCount int64  `json:"asset_count"`
	EventCount uint64 `json:"event_count"`
	FirstBlock uint64 `json:"first_block"`
	LastBlock  uint64 `json:"last_block"`
}

// listAccounts ranks the accounts the index knows about, busiest first.
//
// The q filter is an address prefix, not a search. The index stores an account's
// address and nothing else about it — no names, no labels — so there is nothing else
// here to match on. Turning a name into an address is the caller's job, and what
// arrives back is then a full-length prefix.
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	lo, hi, ok := store.AccountPrefixRange(r.URL.Query().Get("q"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad prefix",
			errors.New("q must be a hex address prefix, at most 40 nibbles"))
		return
	}

	limit := intParam(r, "limit", 50, 1, 500)
	offset := intParam(r, "offset", 0, 0, 1_000_000)
	rows, err := s.d.Store.RankAccounts(r.Context(), chainID, lo, hi, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	out := make([]accountRankJSON, len(rows))
	for i, a := range rows {
		out[i] = accountRankJSON{
			Account:    a.Account.Hex(),
			AssetCount: a.AssetCount,
			EventCount: a.EventCount,
			FirstBlock: a.FirstBlock,
			LastBlock:  a.LastBlock,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id": chainID,
		"limit":    limit,
		"offset":   offset,
		// A full page probably has more behind it. Cheaper, and no less honest, than a
		// second COUNT(DISTINCT account) over the whole rollup for a total that exists
		// only to be printed once.
		"has_more": len(rows) == limit,
		"accounts": out,
	})
}
