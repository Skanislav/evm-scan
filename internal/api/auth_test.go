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
		// The gateway is the whole point of the index being public.
		{http.MethodGet, "/ccip/0x0/0x0"},
	}
	for _, r := range reads {
		if !s.authorized(httptest.NewRequest(r.method, r.path, nil)) {
			t.Errorf("%s %s should never need a token", r.method, r.path)
		}
	}

	spends := []string{"/v1/epochs", "/v1/assets", "/v1/candidates/0xabc/promote"}
	for _, p := range spends {
		if s.authorized(httptest.NewRequest(http.MethodPost, p, nil)) {
			t.Errorf("POST %s must require a token", p)
		}
		withTok := httptest.NewRequest(http.MethodPost, p, nil)
		withTok.Header.Set("Authorization", "Bearer secret")
		if !s.authorized(withTok) {
			t.Errorf("POST %s should accept the right token", p)
		}
		wrong := httptest.NewRequest(http.MethodPost, p, nil)
		wrong.Header.Set("Authorization", "Bearer nope")
		if s.authorized(wrong) {
			t.Errorf("POST %s must reject a wrong token", p)
		}
		bare := httptest.NewRequest(http.MethodPost, p, nil)
		bare.Header.Set("Authorization", "secret")
		if s.authorized(bare) {
			t.Errorf("POST %s must require the Bearer prefix", p)
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
