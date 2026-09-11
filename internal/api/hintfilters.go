package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/store"
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

	_, enc, m, err := s.filterBytes(r.Context(), name, c)
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
	if m.EpochID >= 0 {
		// The epoch whose bonded commitment names this digest. A reader checks it
		// against the chain rather than against us; see cmd/evmscan-verify -filter.
		w.Header().Set("X-Filter-Epoch", strconv.FormatInt(m.EpochID, 10))
	}
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

// filterBytes serves an index filter from the latest finalized epoch when there is
// one, and from the rolling cache otherwise.
//
// The difference matters to a reader. A cache-built filter is current but vouched
// for by nobody: the daemon serving the file also states its digest, so a lying one
// is consistent with itself. An epoch-built filter's digest was fixed inside a
// bonded transaction, which a reader can check against the chain without asking us.
//
// The bytes are rebuilt here rather than stored, and the digest is asserted against
// what the epoch recorded. A mismatch means the rows behind a finalized epoch moved,
// which is a bug in this deployment and not a file to hand out: refuse it rather
// than serve a filter that disagrees with a commitment somebody bonded.
func (s *Server) filterBytes(ctx context.Context, name string, c *hintfilter.Cache) (*hintfilter.Filter, []byte, hintfilter.Manifest, error) {
	chainID, ok := indexFilterChain(name)
	if !ok || s.d.Store == nil {
		return c.Get(ctx)
	}
	e, err := s.d.Store.LatestFinalizedEpoch(ctx, chainID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No finalized epoch yet. The rolling filter is the honest answer, and its
		// epochId of -1 says so.
		return c.Get(ctx)
	case err != nil:
		// Anything else is this deployment failing, not an absent commitment.
		// Degrading quietly here would tell a reader their filter is merely
		// unvouched-for when the truth is that the database is down.
		return nil, nil, hintfilter.Manifest{}, err
	case e.FilterKeccak == (common.Hash{}):
		// Published before filters were committed (migration 0008).
		return c.Get(ctx)
	}

	sets, err := s.d.Store.SnapshotIndex(ctx, chainID, e.ToBlock)
	if err != nil {
		return nil, nil, hintfilter.Manifest{}, err
	}
	rows := make([]hintfilter.AccountAssetSet, len(sets))
	for i, v := range sets {
		rows[i] = hintfilter.AccountAssetSet{Account: v.Account, Assets: v.Assets}
	}
	f, enc, m, err := hintfilter.FromIndex(chainID, rows, e.ToBlock)
	if err != nil {
		return nil, nil, hintfilter.Manifest{}, err
	}
	if m.Keccak256 != e.FilterKeccak.Hex() {
		return nil, nil, hintfilter.Manifest{}, fmt.Errorf(
			"rebuilt filter for epoch %d is %s, but the epoch committed %s",
			e.ID, m.Keccak256, e.FilterKeccak.Hex())
	}
	m.EpochID = e.ID
	return f, enc, m, nil
}

// hintNames lists the served filters in a stable order, so the listing does not
// reshuffle between requests for no reason.
// hintNames is every filter this deployment can serve, not merely every one it has
// been asked for yet.
//
// An index filter is built on demand and cached under its name, so a map of what has
// been made is a map of what somebody has already fetched. Listing that would mean a
// freshly started daemon reports no index filter, a client concludes there is nothing
// to download, and the same filter appears in the listing later only because someone
// guessed the URL — an endpoint whose answer depends on who asked first. The chain
// set is what actually decides which index filters exist, so it is what is listed.
func (s *Server) hintNames() []string {
	seen := map[string]bool{}
	var names []string
	s.hintsMu.Lock()
	for name := range s.hints {
		seen[name] = true
		names = append(names, name)
	}
	s.hintsMu.Unlock()
	if s.d.Store != nil && s.d.Chains != nil {
		for _, e := range s.d.Chains.Entries() {
			if name := indexFilterName(e.ID); !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// indexFilterChain reads the chain id back out of an index filter's name.
func indexFilterChain(name string) (uint64, bool) {
	rest, ok := strings.CutPrefix(name, "index-")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func listURL(lists map[string][]common.Address, name string) string {
	if _, ok := lists[name]; !ok {
		return ""
	}
	return "/v1/hints/" + name + ".json"
}

func (s *Server) hintView(r *http.Request, name string) (hintFilterJSON, error) {
	// Through lookupHint, so that an index filter named by the chain set but never
	// requested is built here rather than reported as missing.
	c, ok := s.lookupHint(name)
	if !ok {
		return hintFilterJSON{}, fmt.Errorf("no filter named %q", name)
	}
	f, enc, m, err := s.filterBytes(r.Context(), name, c)
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

// epochManifest is the document an epoch's URI points at.
//
// A root proves a leaf to whoever already holds it and recovers nothing on its own
// (docs/RECOVERY.md), so the URI has always pointed at the table. This adds the
// filter's digest beside it, which is what lets a reader check the .xorf they
// downloaded against a commitment the publisher bonded rather than against the
// daemon that handed them the file.
//
// The honesty limit is the scheme. Over ipfs:// the URI *is* the content, so the
// bonded transaction fixes this document. Over https:// it fixes only the address:
// the publisher committed to naming that URL, not to what it serves, and a
// deployment vouching for itself is what the digest was meant to get away from.
// docs/PRIVACY.md says so in those words.
//
// One deployment detail matters here: the digest is over the filter's bytes exactly
// as served. A proxy that adds Content-Encoding in front of this daemon changes what
// a reader hashes and every check fails for a reason that looks nothing like the
// cause, so the filter endpoint must not be transparently compressed.
func (s *Server) epochManifest(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "no index", nil)
		return
	}

	// A committed URI normally names its own epoch: Publisher.Build expands {id}
	// once the row exists and before it is published. "latest" is here for the
	// reader who has a name and no epoch number — the ENS record and
	// evmscan-verify both work from the newest finalized epoch — and it is not what
	// commitment_uri should be set to, because a pointer that follows the head
	// stops describing the commitment that carries it.
	var (
		e   store.Epoch
		err error
	)
	raw := r.PathValue("id")
	if raw == "latest" {
		chainID, _, cerr := s.chainOf(r)
		if cerr != nil {
			writeErr(w, http.StatusBadRequest, "bad chain", cerr)
			return
		}
		e, err = s.d.Store.LatestFinalizedEpoch(r.Context(), chainID)
	} else {
		var id int64
		id, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad epoch id", err)
			return
		}
		e, err = s.d.Store.GetEpoch(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such epoch", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, manifestBody(e))
}

// manifestBody is the manifest document. Split from the handler so a test can assert
// the exact keys another program parses.
func manifestBody(e store.Epoch) map[string]any {
	out := map[string]any{
		"epoch":        e.ID,
		"chain_id":     e.ChainID,
		"from_block":   e.FromBlock,
		"to_block":     e.ToBlock,
		"merkle_root":  e.MerkleRoot.Hex(),
		"leaf_count":   e.LeafCount,
		"status":       e.Status,
		"snapshot_url": fmt.Sprintf("/v1/epochs/%d/snapshot", e.ID),
	}
	if e.OnchainID != nil {
		out["onchain_epoch_id"] = *e.OnchainID
	}
	if e.CoverageRoot != (common.Hash{}) {
		out["coverage_root"] = e.CoverageRoot.Hex()
	}
	if e.FilterKeccak != (common.Hash{}) {
		out["index_filter"] = map[string]any{
			"keccak256": e.FilterKeccak.Hex(),
			"to_block":  e.ToBlock,
			"url":       "/v1/hints/" + indexFilterName(e.ChainID) + ".xorf",
			"kind":      "account-token",
		}
	}
	return out
}
