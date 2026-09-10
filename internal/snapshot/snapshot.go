// Package snapshot writes and checks the full index table behind a commitment.
//
// A merkle root is a fingerprint, not a copy: HintRegistry stores 32 bytes for an
// epoch of half a million leaves, and nothing recovers the leaves from it. So an
// epoch whose data lives only in the publisher's Postgres is not recoverable when
// that database is — which makes the commitment auditable by whoever already has
// the answer, and useless to everyone else.
//
// Epoch.uri exists for exactly this ("Pointer to the full index table") and
// IndexPublished emits it, so the pointer is already enumerable from chain logs.
// This package supplies what it should point at.
//
// The format is NDJSON: one header, then the coverage rows, then the leaves, in
// tree order. It is a stream on both ends because the leaf table runs to tens of
// megabytes, and a recovery that needs the whole thing in memory is one that fails
// when it is needed most.
//
// Nothing here has to be trusted. Verify rebuilds both trees from the document and
// hands back the roots; a caller compares them with what the registry holds. That
// is the whole security argument: the host is an availability question, never an
// integrity one, so a snapshot may be mirrored anywhere by anyone.
package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/merkle"
)

// Format is the value of the header's "format" field. The version is part of the
// document because a restore reads files written by builds it has never seen.
const Format = "evmscan-snapshot/1"

// Header is the first line of a snapshot.
type Header struct {
	Format string `json:"format"`
	// ChainID is the chain the index is *about*, which is not necessarily the chain
	// the registry sits on; RegistryChainID is that one. Confusing the two is the
	// standing bug in this codebase, so a snapshot states both.
	ChainID         uint64 `json:"chain_id"`
	FromBlock       uint64 `json:"from_block"`
	ToBlock         uint64 `json:"to_block"`
	Root            string `json:"root"`
	CoverageRoot    string `json:"coverage_root"`
	LeafCount       int64  `json:"leaf_count"`
	AssetCount      int64  `json:"asset_count"`
	OnchainEpochID  *int64 `json:"onchain_epoch_id,omitempty"`
	Registry        string `json:"registry,omitempty"`
	RegistryChainID uint64 `json:"registry_chain_id,omitempty"`
}

// Coverage is one asset and the block range the publisher stood behind for it.
// Together these rows are the asset set, which is the part of an index that cannot
// be recovered from the chain any other way: an asset promoted locally never
// appears in listAssets, and assetKey is a keccak that does not invert.
type Coverage struct {
	Asset     common.Address `json:"asset"`
	FromBlock uint64         `json:"from_block"`
	ToBlock   uint64         `json:"to_block"`
}

// Leaf is one account and the contracts it touched, in the order the tree was
// built from.
type Leaf struct {
	Account common.Address   `json:"account"`
	Assets  []common.Address `json:"assets"`
}

// line is the wire shape. A discriminator keeps the document one stream rather than
// several concatenated ones, so a reader never has to seek.
type line struct {
	T         string           `json:"t"`
	Asset     *common.Address  `json:"asset,omitempty"`
	Account   *common.Address  `json:"account,omitempty"`
	Assets    []common.Address `json:"assets,omitempty"`
	FromBlock uint64           `json:"from_block,omitempty"`
	ToBlock   uint64           `json:"to_block,omitempty"`
}

// Writer streams a snapshot out. Call WriteCoverage for every asset, then WriteLeaf
// for every leaf in tree order, then Close.
type Writer struct {
	w   *bufio.Writer
	enc *json.Encoder
}

// NewWriter writes the header and returns a writer for the body.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	h.Format = Format
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(h); err != nil {
		return nil, fmt.Errorf("write header: %w", err)
	}
	return &Writer{w: bw, enc: enc}, nil
}

func (s *Writer) WriteCoverage(c Coverage) error {
	asset := c.Asset
	return s.enc.Encode(line{T: "coverage", Asset: &asset, FromBlock: c.FromBlock, ToBlock: c.ToBlock})
}

func (s *Writer) WriteLeaf(l Leaf) error {
	acct := l.Account
	return s.enc.Encode(line{T: "leaf", Account: &acct, Assets: l.Assets})
}

func (s *Writer) Close() error { return s.w.Flush() }

// Result is what rebuilding a snapshot's trees produced.
type Result struct {
	Header       Header
	Root         common.Hash
	CoverageRoot common.Hash
	Leaves       int64
	Assets       int64
}

// Matches reports whether the rebuilt roots are the ones the registry committed.
// Both must match: the index root alone would leave the asset set unchecked, and
// the asset set is what a restore rebuilds the scan state from.
func (r Result) Matches(root, coverageRoot common.Hash) bool {
	return r.Root == root && r.CoverageRoot == coverageRoot
}

// Verify reads a snapshot and rebuilds both merkle trees from it.
//
// It recomputes every leaf rather than reading any hash out of the document, so a
// document that disagrees with itself cannot pass: the leaf hash comes from the
// account and its assets, and the coverage leaf from the asset key and its range.
// A snapshot is therefore only as good as the roots a caller checks it against.
func Verify(r io.Reader, onEach func(Coverage, bool)) (Result, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))

	var res Result
	if err := dec.Decode(&res.Header); err != nil {
		return res, fmt.Errorf("read header: %w", err)
	}
	if res.Header.Format != Format {
		return res, fmt.Errorf("unknown snapshot format %q, want %q", res.Header.Format, Format)
	}

	var (
		indexLeaves    []common.Hash
		coverageLeaves []common.Hash
		seenLeaf       bool
	)
	for {
		var ln line
		if err := dec.Decode(&ln); err == io.EOF {
			break
		} else if err != nil {
			return res, fmt.Errorf("row %d: %w", len(indexLeaves)+len(coverageLeaves)+1, err)
		}
		switch ln.T {
		case "coverage":
			if ln.Asset == nil {
				return res, fmt.Errorf("coverage row %d has no asset", len(coverageLeaves)+1)
			}
			// Ordering is not cosmetic: a merkle tree is built from a sequence, so
			// coverage rows interleaved with leaves would put them in the wrong tree.
			if seenLeaf {
				return res, fmt.Errorf("coverage row after a leaf row; sections are ordered")
			}
			key := AssetKey(res.Header.ChainID, *ln.Asset)
			coverageLeaves = append(coverageLeaves, merkle.CoverageLeaf(key, ln.FromBlock, ln.ToBlock))
			if onEach != nil {
				onEach(Coverage{Asset: *ln.Asset, FromBlock: ln.FromBlock, ToBlock: ln.ToBlock}, false)
			}
		case "leaf":
			if ln.Account == nil {
				return res, fmt.Errorf("leaf row %d has no account", len(indexLeaves)+1)
			}
			seenLeaf = true
			indexLeaves = append(indexLeaves,
				merkle.LeafHash(*ln.Account, res.Header.ChainID, merkle.AssetsHash(ln.Assets)))
		default:
			return res, fmt.Errorf("unknown row type %q", ln.T)
		}
	}

	res.Leaves = int64(len(indexLeaves))
	res.Assets = int64(len(coverageLeaves))
	if res.Header.LeafCount != 0 && res.Header.LeafCount != res.Leaves {
		return res, fmt.Errorf("header says %d leaves, document has %d", res.Header.LeafCount, res.Leaves)
	}
	if res.Header.AssetCount != 0 && res.Header.AssetCount != res.Assets {
		return res, fmt.Errorf("header says %d assets, document has %d", res.Header.AssetCount, res.Assets)
	}
	if len(indexLeaves) > 0 {
		res.Root = merkle.Build(indexLeaves).Root()
	}
	if len(coverageLeaves) > 0 {
		res.CoverageRoot = merkle.Build(coverageLeaves).Root()
	}
	return res, nil
}

// Read decodes a whole snapshot into memory. Verify is the streaming path and the
// one a large epoch wants; this is for a restore, which has to hold the rows it is
// about to write anyway.
func Read(r io.Reader) (Header, []Coverage, []Leaf, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))

	var h Header
	if err := dec.Decode(&h); err != nil {
		return h, nil, nil, fmt.Errorf("read header: %w", err)
	}
	if h.Format != Format {
		return h, nil, nil, fmt.Errorf("unknown snapshot format %q, want %q", h.Format, Format)
	}

	var (
		cov    []Coverage
		leaves []Leaf
	)
	for {
		var ln line
		if err := dec.Decode(&ln); err == io.EOF {
			break
		} else if err != nil {
			return h, nil, nil, err
		}
		switch ln.T {
		case "coverage":
			if ln.Asset == nil {
				return h, nil, nil, fmt.Errorf("coverage row %d has no asset", len(cov)+1)
			}
			cov = append(cov, Coverage{Asset: *ln.Asset, FromBlock: ln.FromBlock, ToBlock: ln.ToBlock})
		case "leaf":
			if ln.Account == nil {
				return h, nil, nil, fmt.Errorf("leaf row %d has no account", len(leaves)+1)
			}
			leaves = append(leaves, Leaf{Account: *ln.Account, Assets: ln.Assets})
		default:
			return h, nil, nil, fmt.Errorf("unknown row type %q", ln.T)
		}
	}
	return h, cov, leaves, nil
}

// AssetKey mirrors HintRegistry.assetKey and hintreg.AssetKey. It is duplicated
// here rather than imported so that snapshot does not depend on the registry
// client, which pulls in a node connection this package has no use for.
// hintreg_test asserts the two agree.
func AssetKey(chainID uint64, token common.Address) common.Hash {
	buf := make([]byte, 0, 28)
	for i := 7; i >= 0; i-- {
		buf = append(buf, byte(chainID>>(8*uint(i))))
	}
	buf = append(buf, token[:]...)
	return keccak(buf)
}

// URI expands a commitment_uri template.
//
// {id} is the publisher's own epoch id, which is what GET /v1/epochs/{id}/snapshot
// takes, and {chain} is the indexed chain. Neither is the on-chain epoch id: that
// one is assigned by the registry during publishIndex, which is after the uri has
// already been passed to it. A reader never needs it anyway — it finds the whole
// string in the IndexPublished log.
func URI(template string, chainID uint64, epochID int64) string {
	if template == "" {
		return ""
	}
	return strings.NewReplacer(
		"{id}", fmt.Sprint(epochID),
		"{chain}", fmt.Sprint(chainID),
	).Replace(template)
}

func keccak(b []byte) common.Hash { return crypto.Keccak256Hash(b) }

// Rebuild recomputes both roots from rows already in memory, the way Verify does
// while streaming. A restore has to hold the rows anyway, and must never take a
// root from the header — that is the number an edited document would forge.
func Rebuild(h Header, coverage []Coverage, leaves []Leaf) Result {
	res := Result{Header: h, Leaves: int64(len(leaves)), Assets: int64(len(coverage))}
	if len(coverage) > 0 {
		hs := make([]common.Hash, 0, len(coverage))
		for _, c := range coverage {
			hs = append(hs, merkle.CoverageLeaf(AssetKey(h.ChainID, c.Asset), c.FromBlock, c.ToBlock))
		}
		res.CoverageRoot = merkle.Build(hs).Root()
	}
	if len(leaves) > 0 {
		hs := make([]common.Hash, 0, len(leaves))
		for _, l := range leaves {
			hs = append(hs, merkle.LeafHash(l.Account, h.ChainID, merkle.AssetsHash(l.Assets)))
		}
		res.Root = merkle.Build(hs).Root()
	}
	return res
}

// Open reads a snapshot from a path, "-" for stdin, or an http(s) URL. A recovery
// normally starts from the uri in an IndexPublished log, which is a URL; the same
// file on disk has to work when the network is what broke.
func Open(ctx context.Context, source string) (io.ReadCloser, error) {
	switch {
	case source == "-":
		return io.NopCloser(os.Stdin), nil
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: %s", source, resp.Status)
		}
		return resp.Body, nil
	case strings.HasPrefix(source, "ipfs://"):
		return nil, fmt.Errorf("ipfs:// is not fetched directly; pass a gateway URL for %s", source)
	default:
		return os.Open(source)
	}
}
