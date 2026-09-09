package chain

import (
	"errors"
	"testing"
)

// TestParseEndpointClassifiesLocality is the test that matters most here: the
// require_local_node guarantee is only as good as this classification.
func TestParseEndpointClassifiesLocality(t *testing.T) {
	cases := []struct {
		raw       string
		transport Transport
		local     bool
		streaming bool
	}{
		{"/tmp/devchain/geth.ipc", TransportIPC, true, true},
		{"./geth.ipc", TransportIPC, true, true},
		{"ws://127.0.0.1:8546", TransportWS, true, true},
		{"ws://localhost:8546", TransportWS, true, true},
		{"ws://[::1]:8546", TransportWS, true, true},
		{"http://127.0.0.1:8545", TransportHTTP, true, false},
		{"https://mainnet.example.com", TransportHTTP, false, false},
		{"wss://rpc.example.com", TransportWS, false, true},
		{"http://10.0.0.5:8545", TransportHTTP, false, false},
	}

	for _, tc := range cases {
		ep, err := ParseEndpoint(tc.raw)
		if err != nil {
			t.Errorf("%s: %v", tc.raw, err)
			continue
		}
		if ep.Transport != tc.transport {
			t.Errorf("%s: transport = %s, want %s", tc.raw, ep.Transport, tc.transport)
		}
		if ep.Local != tc.local {
			t.Errorf("%s: local = %v, want %v", tc.raw, ep.Local, tc.local)
		}
		if ep.Streaming != tc.streaming {
			t.Errorf("%s: streaming = %v, want %v", tc.raw, ep.Streaming, tc.streaming)
		}
	}
}

// TestParseEndpointDoesNotResolveHostnames: a hostname that happens to resolve to
// loopback today is not a durable guarantee that the node is ours, so it must not
// count as local.
func TestParseEndpointDoesNotResolveHostnames(t *testing.T) {
	ep, err := ParseEndpoint("http://my-node.internal:8545")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Local {
		t.Error("a non-literal hostname was classified as local")
	}
}

func TestParseEndpointRejectsEmpty(t *testing.T) {
	if _, err := ParseEndpoint(""); err == nil {
		t.Error("expected an error for an empty endpoint")
	}
}

func TestHTTPIsNeverStreaming(t *testing.T) {
	// eth_subscribe cannot ride on plain HTTP; the follower must fall back to polling.
	for _, raw := range []string{"http://127.0.0.1:8545", "https://rpc.example.com"} {
		ep, err := ParseEndpoint(raw)
		if err != nil {
			t.Fatal(err)
		}
		if ep.Streaming {
			t.Errorf("%s: reported as streaming", raw)
		}
	}
}

func TestIsRangeLimitMatchesProviderErrors(t *testing.T) {
	// These are the (unstandardised) shapes providers use to say "narrow your window".
	// Misclassifying one as fatal turns a recoverable sweep into a hard failure.
	limits := []string{
		"query returned more than 10000 results",
		"Log response size exceeded",
		"eth_getLogs block range too large",
		"limit exceeded",
		"error code: -32005",
		// Observed against a hosted deployment: Ankr capping a response, Helios
		// failing to serialise its own, and Helios's wrapper for an upstream request
		// that never came back. All three are answered by a narrower window, and all
		// three used to abort the sweep instead.
		"Response is too big",
		`Error serializing response: Error("Memory capacity exceeded")`,
		"error sending request for url (https://rpc.example/eth/KEY)",
	}
	for _, s := range limits {
		if !isRangeLimit(errors.New(s)) {
			t.Errorf("%q should be treated as a range limit", s)
		}
	}

	fatal := []string{
		"connection refused",
		"context deadline exceeded",
		"invalid argument",
	}
	for _, s := range fatal {
		if isRangeLimit(errors.New(s)) {
			t.Errorf("%q should not be treated as a range limit", s)
		}
	}
}

func TestChunkOptsDefaults(t *testing.T) {
	o := ChunkOpts{}.withDefaults()
	if o.Max == 0 || o.Min == 0 {
		t.Fatalf("defaults left a zero bound: %+v", o)
	}
	if o.Min > o.Max {
		t.Errorf("min %d exceeds max %d", o.Min, o.Max)
	}

	// A caller that sets only Min must not end up with Min > Max.
	o2 := ChunkOpts{Max: 10, Min: 500}.withDefaults()
	if o2.Min > o2.Max {
		t.Errorf("min %d exceeds max %d after clamping", o2.Min, o2.Max)
	}
}
