package api

import (
	"encoding/json"
	"testing"
)

// The point of serving these is that a client can run the lens itself against its
// own RPC. That only works if the calling convention is complete: creation code to
// prefix, the parameters to encode after it, and the shape to decode back. A
// half-described convention fails at the decode, where the symptom is nonsense
// rather than an error, so all three are asserted here.
func TestLensArtifactsDescribeTheWholeCall(t *testing.T) {
	got, err := buildLensArtifacts()
	if err != nil {
		t.Fatalf("buildLensArtifacts: %v", err)
	}
	for _, key := range []string{"asset", "price"} {
		a, ok := got[key]
		if !ok {
			t.Fatalf("%s lens is not served", key)
		}
		if len(a.Creation) < 100 || a.Creation[:2] != "0x" {
			t.Errorf("%s: creation code is not 0x-prefixed bytecode", key)
		}
		var req, rep []map[string]any
		if err := json.Unmarshal(a.Request, &req); err != nil {
			t.Fatalf("%s request is not ABI parameters: %v", key, err)
		}
		if err := json.Unmarshal(a.Reply, &rep); err != nil {
			t.Fatalf("%s reply is not ABI parameters: %v", key, err)
		}
		if len(req) != 1 || req[0]["type"] != "tuple" {
			t.Errorf("%s: want a single request tuple, got %d params", key, len(req))
		}
		if len(rep) != 1 || rep[0]["type"] != "tuple" {
			t.Errorf("%s: want a single reply tuple, got %d params", key, len(rep))
		}
		// A client batches against these, so zero would mean unlimited and be wrong.
		if a.MaxReplyBytes == 0 || a.MaxPayloadBytes == 0 {
			t.Errorf("%s: consensus limits must be stated", key)
		}
	}
}
