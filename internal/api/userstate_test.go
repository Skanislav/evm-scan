package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Skanislav/evm-scan/internal/userstate"
)

func TestStateAuthorizationBoundary(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		guard        bool
	}{{"POST", "/v1/accounts/0x01/state", false}, {"DELETE", "/v1/accounts/0x01/state", true}, {"POST", "/v1/state/checkpoints", true}, {"POST", "/v1/state/revisions/0x01", true}} {
		if got := guarded(httptest.NewRequest(tc.method, tc.path, nil)); got != tc.guard {
			t.Fatalf("%s %s guarded=%t", tc.method, tc.path, got)
		}
	}
}
func TestStateRejectsBeforeStore(t *testing.T) {
	raw, err := os.ReadFile("../userstate/testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Snapshot userstate.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []string{`{}`, `{"snapshot":{"version":2},"generation":"0"}`, `{"unknown":1}`, strings.Repeat(" ", userstate.MaxRequestBytes) + `{}`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc))
		r.SetPathValue("address", f.Snapshot.Account.Hex())
		w := httptest.NewRecorder()
		s := &Server{}
		s.postUserState(w, r)
		if w.Code != 400 {
			t.Fatal("expected pre-store rejection", w.Code)
		}
	}
	req := struct {
		Snapshot   userstate.Snapshot `json:"snapshot"`
		Generation string             `json:"generation"`
	}{f.Snapshot, "0"}
	b, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/", bytes.NewReader(b))
	r.SetPathValue("address", "0x0000000000000000000000000000000000000001")
	w := httptest.NewRecorder()
	(&Server{}).postUserState(w, r)
	if w.Code != 400 {
		t.Fatal("account mismatch not rejected")
	}
}
