package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/userstate"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func stateHash(raw string) (common.Hash, error) {
	b, err := hexutil.Decode(raw)
	if err != nil || len(b) != 32 {
		return common.Hash{}, errors.New("expected 32-byte hex hash")
	}
	return common.BytesToHash(b), nil
}
func stateDecode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, userstate.MaxRequestBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func stateError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) {
		code = 404
	}
	if errors.Is(err, store.ErrStateConflict) {
		code = 409
	}
	writeErr(w, code, "could not read or update state", err)
}
func (s *Server) getUserState(w http.ResponseWriter, r *http.Request) {
	a, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, 400, err.Error(), nil)
		return
	}
	v, err := s.d.Store.UserState(r.Context(), a)
	if err != nil {
		stateError(w, err)
		return
	}
	p, err := s.d.Store.StateProjection(r.Context(), a)
	if err != nil {
		stateError(w, err)
		return
	}
	after, err := s.d.Store.UserState(r.Context(), a)
	if err != nil {
		stateError(w, err)
		return
	}
	if after.Generation != v.Generation {
		stateError(w, store.ErrStateConflict)
		return
	}
	writeJSON(w, 200, map[string]any{"state": v, "projection": p})
}
func (s *Server) postUserState(w http.ResponseWriter, r *http.Request) {
	a, err := parseAddress(r.PathValue("address"))
	if err != nil {
		writeErr(w, 400, err.Error(), nil)
		return
	}
	var req struct {
		Snapshot   userstate.Snapshot `json:"snapshot"`
		Generation string             `json:"generation"`
	}
	if err := stateDecode(w, r, &req); err != nil {
		writeErr(w, 400, "invalid state request", err)
		return
	}
	if req.Snapshot.Account != a {
		writeErr(w, 400, "state account does not match URL", nil)
		return
	}
	if _, _, err := req.Snapshot.Validate(); err != nil {
		writeErr(w, 400, "invalid signed state", err)
		return
	}
	if err := s.d.Store.PutUserState(r.Context(), req.Snapshot, req.Generation); err != nil {
		if errors.Is(err, store.ErrStateConflict) {
			stateError(w, err)
		} else {
			writeErr(w, 400, "state not accepted", err)
		}
		return
	}
	v, err := s.d.Store.UserState(r.Context(), a)
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) getStateRevision(w http.ResponseWriter, r *http.Request) {
	id, err := stateHash(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error(), nil)
		return
	}
	v, err := s.d.Store.StateRevision(r.Context(), id)
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) getStateCheckpoint(w http.ResponseWriter, r *http.Request) {
	h, err := stateHash(r.PathValue("root"))
	if err != nil {
		writeErr(w, 400, err.Error(), nil)
		return
	}
	c, err := s.d.Store.StateCheckpoint(r.Context(), h)
	if err != nil {
		stateError(w, err)
		return
	}
	if raw := r.URL.Query().Get("account"); raw != "" {
		a, err := parseAddress(raw)
		if err != nil {
			writeErr(w, 400, err.Error(), nil)
			return
		}
		t, err := userstate.Aggregate(c.Accounts)
		if err != nil {
			stateError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"root": c.Root, "proof": t.Proof(userstate.AccountKey(a))})
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) buildStateCheckpoint(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.Store.BuildStateCheckpoint(r.Context())
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) getStateStatus(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.d.Store.StatePublications(r.Context())
	if err != nil {
		stateError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chain_id": "11155111", "resolver": s.d.StateResolver, "node": s.d.StateNode, "name": s.d.StateName, "publications": jobs})
}
