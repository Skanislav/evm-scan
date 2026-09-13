package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postAssetCommitWith(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/accounts/0x0000000000000000000000000000000000000001/asset-commit", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Every case must fail before storage: this route is intentionally reachable without
// the operator token, so malformed input must never get as far as a write.
func TestAssetCommitRefusesBeforeTheStore(t *testing.T) {
	s := New(Deps{})
	cases := []struct {
		name, body, want string
	}{
		{"zero chain", `{"assets":[{"chain_id":0,"address":"0x0000000000000000000000000000000000000001"}],"deadline":"9999999999","signature":"0x00"}`, "chain_id"},
		{"bad asset", `{"assets":[{"chain_id":1,"address":"0x1"}],"deadline":"9999999999","signature":"0x00"}`, "address"},
		{"duplicate pair", `{"assets":[{"chain_id":1,"address":"0x0000000000000000000000000000000000000001"},{"chain_id":1,"address":"0x0000000000000000000000000000000000000001"}],"deadline":"9999999999","signature":"0x00"}`, "appears twice"},
		{"expired", `{"assets":[],"deadline":"1","signature":"0x00"}`, "expired"},
		{"short signature", `{"assets":[],"deadline":"9999999999","signature":"0x00"}`, "65 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postAssetCommitWith(t, s, tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status/body = %d %q; want 400 containing %q", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
