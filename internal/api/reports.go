package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Skanislav/evm-scan/internal/store"
)

// Reports against an indexed asset.
//
// Separate from the candidate spam verdict because the two decide different things.
// A candidate verdict decides whether to spend a backfill on a contract nobody has
// paid for. This decides where a contract that has *already* been indexed appears in
// a list — and an asset someone bought through requestIndexing never was a candidate,
// so without this a funded scam carries no verdict of any kind.
//
// A report cannot revoke, un-index or refund anything, and is not meant to: the
// funding bought a backfill that has already run and coverage that is already in a
// root. What it does is decline to put the contract first. Money buys indexing; it
// does not buy position.
//
// One report is enough, because ordering is not adjudication. Deranking a good
// contract costs it a place in a list and costs a reader one extra balance read;
// ranking a scam costs somebody seeing it at the top of their wallet. Those are not
// the same mistake, so they do not get the same bar — which is also why
// candidates.spam_at, where the decision is whether to spend a backfill, keeps a
// high one.
func (s *Server) reportAsset(w http.ResponseWriter, r *http.Request) {
	s.assetReport(w, r, true)
}

// clearAssetReport is the undo. A POST rather than a DELETE for the same reason the
// candidate verdicts use one: the CORS preflight this server answers allows GET,
// POST and OPTIONS only, and the auth guard covers POSTs.
func (s *Server) clearAssetReport(w http.ResponseWriter, r *http.Request) {
	s.assetReport(w, r, false)
}

func (s *Server) assetReport(w http.ResponseWriter, r *http.Request, report bool) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	addr, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad address", err)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// A missing or unparseable body is not a failure: the reason is optional and a
	// report with no explanation still orders the list.
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)

	if !report {
		if err := s.d.Store.ClearAssetReports(r.Context(), chainID, addr); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not clear the reports", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"address": addr, "reports": 0})
		return
	}

	n, err := s.d.Store.ReportAsset(r.Context(), chainID, addr, body.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such asset on this chain", err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record the report", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"address": addr, "reports": n})
}
