package api

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Skanislav/evm-scan/internal/snapshot"
	"github.com/Skanislav/evm-scan/internal/store"
)

// epochSnapshot streams the full index table behind one commitment.
//
// This is what Epoch.uri should point at. The registry holds a root, and a root
// proves a leaf someone already has — it recovers nothing. Without a published
// table, an index survives exactly as long as the publisher's database, and the
// commitment is auditable only by the party that needs auditing.
//
// The response is not trusted and does not need to be: every leaf here is
// recomputable, so a reader rebuilds both trees and compares them with the roots on
// chain (evmscan-verify -snapshot). That makes hosting an availability problem
// rather than an integrity one, which is what lets anyone mirror the file.
//
// It is deliberately open even when AuthToken is set. The token guards endpoints
// that spend the deployment's money; this one exists to be fetched by strangers,
// and a recovery path nobody can reach is not a recovery path.
func (s *Server) epochSnapshot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad epoch id", err)
		return
	}

	ctx := r.Context()
	e, err := s.d.Store.GetEpoch(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "epoch not found", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed", err)
		return
	}

	coverage, err := s.d.Store.EpochCoverage(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read coverage failed", err)
		return
	}

	h := snapshot.Header{
		ChainID:         e.ChainID,
		FromBlock:       e.FromBlock,
		ToBlock:         e.ToBlock,
		Root:            e.MerkleRoot.Hex(),
		CoverageRoot:    e.CoverageRoot.Hex(),
		LeafCount:       e.LeafCount,
		AssetCount:      int64(len(coverage)),
		OnchainEpochID:  e.OnchainID,
		RegistryChainID: s.d.RegistryChainID,
	}
	if s.d.Registry != nil {
		h.Registry = s.d.Registry.Address().Hex()
	}

	// The leaf table runs to tens of megabytes on a real chain, so it goes out as a
	// stream. Headers are committed before the first row: once bytes are on the wire
	// an error cannot become a status code, which is why the coverage read above
	// happens first.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"evmscan-epoch-%d-chain-%d.ndjson\"", id, e.ChainID))

	var out io.Writer = w
	var gz *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz = gzip.NewWriter(w)
		out = gz
	}
	w.WriteHeader(http.StatusOK)

	sw, err := snapshot.NewWriter(out, h)
	if err != nil {
		s.d.Log.Error("snapshot header failed", "epoch", id, "err", err)
		return
	}
	for _, c := range coverage {
		if err := sw.WriteCoverage(snapshot.Coverage{
			Asset: c.Asset, FromBlock: c.FromBlock, ToBlock: c.ToBlock,
		}); err != nil {
			s.d.Log.Error("snapshot coverage failed", "epoch", id, "err", err)
			return
		}
	}
	err = s.d.Store.EachEpochLeaf(ctx, id, func(l store.EpochLeaf) error {
		return sw.WriteLeaf(snapshot.Leaf{Account: l.Account, Assets: l.Assets})
	})
	if err != nil {
		// The client gets a truncated document, which fails verification rather than
		// passing quietly — the roots will not match a partial leaf set.
		s.d.Log.Error("snapshot stream failed", "epoch", id, "err", err)
		return
	}
	if err := sw.Close(); err != nil {
		s.d.Log.Error("snapshot flush failed", "epoch", id, "err", err)
		return
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			s.d.Log.Error("snapshot gzip close failed", "epoch", id, "err", err)
		}
	}
}
