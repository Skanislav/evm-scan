package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
)

// Hint filters are the private half of the read path.
//
// Every other way to ask this deployment what an account holds puts the account in
// a URL or in ERC-3668 calldata. That gives unforgeability — the registry's callback
// re-derives the leaf and checks it against the on-chain root, so a gateway can
// withhold an answer but cannot forge one — and it gives no privacy at all: the
// operator learns exactly who asked, every time.
//
// A filter closes that. It is a static file, identical for every visitor, so
// fetching it says nothing about who is fetching. The reader downloads it once and
// tests their own address locally, and the daemon never learns the question. The
// answer that follows comes from the reader's own node through the deployless lens,
// so the daemon is out of the loop entirely.
//
// Nothing served here is trusted by anything. A filter says where to look; the
// live read says what is there. A stale or lying filter costs a wasted balanceOf.

// hintFilterJSON describes one available filter.
type hintFilterJSON struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	ChainID uint64 `json:"chain_id"`
	KeyKind string `json:"key_kind"`
	Kind    uint8  `json:"kind"`
	Struct  string `json:"structure"`
	// ListURL is the enumerable form, when there is one. A filter can be tested but
	// never walked, so a reader narrowing privately needs the candidates from
	// somewhere.
	ListURL string `json:"list_url,omitempty"`
	Count   uint64 `json:"count"`
	Bytes   int    `json:"bytes"`
	Blinded bool   `json:"blinded"`
	ToBlock uint64 `json:"to_block,omitempty"`
	Keccak  string `json:"keccak256"`
	SHA256  string `json:"sha256"`
	// Domain and subkey derivation, so a client can derive keys without having the
	// constants transcribed into it. Copying them by hand is how a reader ends up
	// computing keys nobody else computes.
	Derivation hintDerivationJSON `json:"derivation"`
}

type hintDerivationJSON struct {
	Subkey string `json:"subkey"`
	Key    string `json:"key"`
	Domain string `json:"domain"`
}

func derivation() hintDerivationJSON {
	return hintDerivationJSON{
		Subkey: `keccak256(secret || "evmscan/xorf/v1" || uint64be(chainId) || uint8(kind))`,
		Key:    `uint64be(keccak256(subkey || part...)[0:8])`,
		Domain: "evmscan/xorf/v1",
	}
}

// listHints reports the filters this deployment serves.
func (s *Server) listHints(w http.ResponseWriter, r *http.Request) {
	out := []hintFilterJSON{}
	for _, name := range s.hintNames() {
		v, err := s.hintView(r, name)
		if err != nil {
			// One chain with nothing indexed should not blank the whole listing.
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"filters": out})
}

// serveHintFilter returns one filter's bytes.
func (s *Server) serveHintFilter(w http.ResponseWriter, r *http.Request) {
	// A ServeMux wildcard has to span a whole path segment, so the extension is
	// matched here rather than in the pattern.
	file := r.PathValue("file")

	// The enumerable form. A filter can be tested but never walked, so a reader
	// doing the private narrowing needs the candidate addresses from somewhere —
	// and fetching them from a third party would put back the observer this whole
	// path exists to remove.
	if name, ok := strings.CutSuffix(file, ".json"); ok {
		s.serveHintList(w, r, name)
		return
	}

	name, ok := strings.CutSuffix(file, ".xorf")
	if !ok {
		writeErr(w, http.StatusNotFound, "filters are served as .xorf or .json", nil)
		return
	}
	c, ok := s.lookupHint(name)
	if !ok {
		// Resolve against the fixed map and never against the filesystem. This is a
		// user-controlled path segment, and a handler that joins one onto a
		// directory is the textbook traversal.
		writeErr(w, http.StatusNotFound, "no such filter", nil)
		return
	}

	_, enc, m, err := c.Get(r.Context())
	if errors.Is(err, hintfilter.ErrEmpty) {
		writeErr(w, http.StatusNotFound, "nothing is indexed on this chain yet", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not build the filter", err)
		return
	}

	// The digest is the natural ETag: the format is deterministic, so identical
	// content always produces an identical tag, and a rebuild that changed nothing
	// costs the client no bytes.
	etag := `"` + m.Keccak256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Filter-To-Block", strconv.FormatUint(m.ToBlock, 10))
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(enc)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(enc)
}

// serveHintList returns the addresses behind a token filter, for a reader that
// needs to iterate candidates before testing them.
//
// Only token filters have one. An index filter is keyed on (account, token) pairs,
// and publishing those as a list would hand out the entire social graph the filter
// is careful not to reveal — the filter form is the point.
func (s *Server) serveHintList(w http.ResponseWriter, r *http.Request, name string) {
	addrs, ok := s.hintLists[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "no enumerable list for this filter", nil)
		return
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.Hex()
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "count": len(out), "tokens": out})
}

// hintNames lists the served filters in a stable order, so the listing does not
// reshuffle between requests for no reason.
func (s *Server) hintNames() []string {
	names := make([]string, 0, len(s.hints))
	for name := range s.hints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func listURL(lists map[string][]common.Address, name string) string {
	if _, ok := lists[name]; !ok {
		return ""
	}
	return "/v1/hints/" + name + ".json"
}

func (s *Server) hintView(r *http.Request, name string) (hintFilterJSON, error) {
	c := s.hints[name]
	f, enc, m, err := c.Get(r.Context())
	if err != nil {
		return hintFilterJSON{}, err
	}
	return hintFilterJSON{
		Name: name, URL: "/v1/hints/" + name + ".xorf",
		ChainID: f.ChainID, KeyKind: f.Kind.String(), Kind: uint8(f.Kind),
		Struct: f.Structure.String(), Count: f.Count(), Bytes: len(enc),
		ListURL: listURL(s.hintLists, name),
		Blinded: f.Blinded, ToBlock: m.ToBlock,
		Keccak: m.Keccak256, SHA256: m.SHA256,
		Derivation: derivation(),
	}, nil
}
