package chain

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Transport is the wire protocol used to reach a node.
type Transport string

const (
	TransportIPC  Transport = "ipc"
	TransportWS   Transport = "ws"
	TransportHTTP Transport = "http"
)

// Endpoint describes how we reach an execution client, and crucially whether that
// client is ours.
//
// The whole point of this project is that asset discovery should not require trusting
// a third-party RPC provider: we run our own snap-synced node and read it directly.
// Endpoint carries the locality signal so that configuration can *enforce* that
// (see config.RequireLocalNode) instead of merely documenting it.
type Endpoint struct {
	Raw       string
	Transport Transport
	// Local reports whether this endpoint is a unix socket or a loopback address.
	Local bool
	// Streaming reports whether the transport supports eth_subscribe. HTTP does not.
	Streaming bool
}

// ParseEndpoint classifies a node endpoint. Anything that is not a ws:// or http://
// URL is treated as an IPC socket path, which is how geth's --ipcpath is addressed.
func ParseEndpoint(raw string) (Endpoint, error) {
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty node endpoint")
	}

	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "ws://"), strings.HasPrefix(lower, "wss://"):
		local, err := loopbackURL(raw)
		if err != nil {
			return Endpoint{}, err
		}
		return Endpoint{Raw: raw, Transport: TransportWS, Local: local, Streaming: true}, nil

	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		local, err := loopbackURL(raw)
		if err != nil {
			return Endpoint{}, err
		}
		// HTTP cannot carry eth_subscribe; the follower falls back to polling.
		return Endpoint{Raw: raw, Transport: TransportHTTP, Local: local, Streaming: false}, nil

	default:
		// A filesystem path: geth IPC. Always local by construction.
		return Endpoint{Raw: raw, Transport: TransportIPC, Local: true, Streaming: true}, nil
	}
}

func loopbackURL(raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, fmt.Errorf("parse endpoint %q: %w", raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return false, fmt.Errorf("endpoint %q has no host", raw)
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	// Only a literal loopback IP counts. A hostname that merely resolves to 127.0.0.1
	// today is not a durable guarantee that the node is ours, so we do not resolve it.
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	return false, nil
}

func (e Endpoint) String() string {
	locality := "remote"
	if e.Local {
		locality = "local"
	}
	return fmt.Sprintf("%s (%s, %s)", e.Redacted(), e.Transport, locality)
}

// Redacted renders the endpoint with any credential removed.
//
// Providers put the API key straight in the URL — a path segment on dRPC and Ankr,
// a query parameter elsewhere — so the endpoint string is a secret, and it was
// being served by an unauthenticated /v1/status and written to logs. Anyone who
// could reach the status page could take the key and spend the operator's quota.
//
// The rule is deliberately blunt: keep the scheme, host and the first path segment
// (which names the network on most providers, and is not secret), and drop
// everything after it along with every query value. Over-redacting an endpoint
// costs nothing; under-redacting one hands out a credential.
func (e Endpoint) Redacted() string {
	if e.Transport == TransportIPC {
		return e.Raw
	}
	u, err := url.Parse(e.Raw)
	if err != nil {
		// Unparseable, so nothing can be said about which part is the secret.
		return "(redacted)"
	}

	// Assembled by hand rather than through url.String, which percent-escapes the
	// marker and turns a readable endpoint into line noise.
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		b.WriteString("***@")
	}
	b.WriteString(u.Host)

	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] == "":
		// no path at all
	case len(segs) == 1:
		// A single segment may itself be the key (…/<key>), so keep it only when it
		// is short enough to be a network name.
		if len(segs[0]) > 24 {
			b.WriteString("/***")
		} else {
			b.WriteString("/" + segs[0])
		}
	default:
		b.WriteString("/" + segs[0] + "/***")
	}

	if u.RawQuery != "" {
		keys := make([]string, 0, 4)
		for k := range u.Query() {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("?")
		for i, k := range keys {
			if i > 0 {
				b.WriteString("&")
			}
			b.WriteString(k + "=***")
		}
	}
	return b.String()
}
