package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorizedGuardsOnlySpendingEndpoints(t *testing.T) {
	s := &Server{d: Deps{AuthToken: "secret"}}

	reads := []struct{ method, path string }{
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/assets"},
		{http.MethodGet, "/v1/candidates"},
		{http.MethodGet, "/v1/epochs"},
		{http.MethodGet, "/v1/accounts/0x0/contracts"},
		{http.MethodGet, "/v1/chains"},
		// Resolving a name costs one eth_call and reports what anybody could read
		// off mainnet themselves. Gating it would only make the UI worse.
		{http.MethodGet, "/v1/chains/resolve"},
		// The gateway is the whole point of the index being public.
		{http.MethodGet, "/ccip/0x0/0x0"},
	}
	for _, r := range reads {
		if !s.authorized(httptest.NewRequest(r.method, r.path, nil)) {
			t.Errorf("%s %s should never need a token", r.method, r.path)
		}
	}

	// Adding a chain is a per-block RPC bill from then on, and PATCHing one to
	// trust: verified puts the publisher's bond behind a node we do not run —
	// the single highest-value thing this token guards.
	spends := []struct{ method, path string }{
		{http.MethodPost, "/v1/epochs"},
		{http.MethodPost, "/v1/assets"},
		{http.MethodPost, "/v1/candidates/0xabc/promote"},
		{http.MethodPost, "/v1/chains"},
		{http.MethodPatch, "/v1/chains/8453"},
		{http.MethodDelete, "/v1/chains/8453"},
	}
	for _, sp := range spends {
		if s.authorized(httptest.NewRequest(sp.method, sp.path, nil)) {
			t.Errorf("%s %s must require a token", sp.method, sp.path)
		}
		withTok := httptest.NewRequest(sp.method, sp.path, nil)
		withTok.Header.Set("Authorization", "Bearer secret")
		if !s.authorized(withTok) {
			t.Errorf("%s %s should accept the right token", sp.method, sp.path)
		}
		wrong := httptest.NewRequest(sp.method, sp.path, nil)
		wrong.Header.Set("Authorization", "Bearer nope")
		if s.authorized(wrong) {
			t.Errorf("%s %s must reject a wrong token", sp.method, sp.path)
		}
		bare := httptest.NewRequest(sp.method, sp.path, nil)
		bare.Header.Set("Authorization", "secret")
		if s.authorized(bare) {
			t.Errorf("%s %s must require the Bearer prefix", sp.method, sp.path)
		}
	}
}

func TestNoTokenConfiguredLeavesEverythingOpen(t *testing.T) {
	s := &Server{d: Deps{}}
	for _, p := range []string{"/v1/epochs", "/v1/assets", "/v1/candidates/0xabc/promote"} {
		if !s.authorized(httptest.NewRequest(http.MethodPost, p, nil)) {
			t.Errorf("POST %s should be open when no token is configured", p)
		}
	}
}
