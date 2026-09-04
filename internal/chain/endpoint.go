package chain

import (
	"fmt"
	"net"
	"net/url"
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
	return fmt.Sprintf("%s (%s, %s)", e.Raw, e.Transport, locality)
}
