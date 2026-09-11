package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// runFilter checks a downloaded .xorf against the digest an epoch committed.
//
// This is the half that makes the commitment worth anything. A filter served over
// HTTP is vouched for by the host that served it: ask the same daemon for the file
// and for its digest and a lying one is consistent with itself. Here the digest
// comes from the chain instead — the epoch's URI was named inside the bonded
// publishIndex transaction, and the manifest behind it carries what the publisher
// stood behind.
//
// Without a registry this still reports what the file is and what it says about
// itself, which is worth having when checking a build by hand.
func runFilter(ctx context.Context, source, nodeURL, registryAddr string, chainID uint64) error {
	f, raw, err := loadFilter(ctx, source)
	if err != nil {
		return err
	}
	digest := crypto.Keccak256Hash(raw)

	fmt.Printf("filter    %s\n", source)
	fmt.Printf("  chain   %d\n", f.ChainID)
	fmt.Printf("  kind    %s, %s\n", f.Kind, f.Structure)
	fmt.Printf("  keys    %d in %d bytes (%.2f bytes/key)\n",
		f.Count(), len(raw), float64(len(raw))/float64(max64(f.Count(), 1)))
	fmt.Printf("  toBlock %d\n", f.ToBlock)
	fmt.Printf("  keccak  %s\n", digest.Hex())
	if f.Blinded {
		// Nothing here can read a blinded filter, and saying so beats printing a
		// count of keys nobody can match.
		fmt.Printf("  blinded: only the holder of its secret can test this file\n")
	}

	if nodeURL == "" || registryAddr == "" {
		fmt.Printf("\nno -node/-registry: nothing checked this against the chain\n")
		return nil
	}

	src, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		return err
	}
	defer src.Close()

	client, err := hintreg.NewClient(src, common.HexToAddress(registryAddr))
	if err != nil {
		return err
	}
	if chainID == 0 {
		chainID = f.ChainID
	}

	found, id, err := client.LatestFinalizedEpoch(ctx, chainID)
	if err != nil {
		return fmt.Errorf("read the latest finalized epoch: %w", err)
	}
	if !found {
		return fmt.Errorf("chain %d has no finalized epoch, so nothing on-chain names a filter yet", chainID)
	}
	e, err := client.GetEpoch(ctx, id)
	if err != nil {
		return fmt.Errorf("read epoch %d: %w", id, err)
	}
	fmt.Printf("\non-chain epoch %d for chain %d\n", id, chainID)
	fmt.Printf("  range   %d-%d\n", e.FromBlock, e.ToBlock)
	fmt.Printf("  root    %s\n", e.Root.Hex())
	fmt.Printf("  uri     %s\n", e.URI)

	if e.URI == "" {
		return fmt.Errorf("the epoch committed no URI, so there is no manifest to check against")
	}
	if !strings.HasPrefix(e.URI, "ipfs://") {
		// Worth saying every time rather than in a footnote. Over https the bonded
		// transaction fixes the address and not the bytes: the publisher committed
		// to naming that URL, not to what it serves today.
		fmt.Printf("  note    this URI is not content-addressed; the chain fixes where to\n")
		fmt.Printf("          look, not what is found there\n")
	}

	want, err := manifestDigest(ctx, e.URI)
	if err != nil {
		return err
	}
	fmt.Printf("  filter  %s\n", want.Hex())

	if want != digest {
		return fmt.Errorf("MISMATCH: the file hashes to %s, the epoch's manifest says %s",
			digest.Hex(), want.Hex())
	}
	if f.ToBlock != e.ToBlock {
		return fmt.Errorf("MISMATCH: the file says block %d, the epoch covers to %d",
			f.ToBlock, e.ToBlock)
	}
	fmt.Printf("\nOK: this file is the one epoch %d committed, as of block %d\n", id, e.ToBlock)
	return nil
}

// manifestDigest fetches the epoch's manifest and reads the filter digest out of it.
func manifestDigest(ctx context.Context, uri string) (common.Hash, error) {
	if !strings.HasPrefix(uri, "http://") && !strings.HasPrefix(uri, "https://") {
		return common.Hash{}, fmt.Errorf("cannot fetch %q: only http(s) URIs are followed here", uri)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return common.Hash{}, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return common.Hash{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return common.Hash{}, fmt.Errorf("fetch %s: %s", uri, resp.Status)
	}

	var m struct {
		IndexFilter struct {
			Keccak256 string `json:"keccak256"`
		} `json:"index_filter"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return common.Hash{}, fmt.Errorf("parse the manifest at %s: %w", uri, err)
	}
	if m.IndexFilter.Keccak256 == "" {
		// The usual cause is a commitment_uri pointing straight at the snapshot
		// rather than at the manifest beside it, so say that rather than leaving
		// an operator to guess at a missing JSON key.
		return common.Hash{}, fmt.Errorf(
			"the document at %s names no index filter; the epoch's URI should point at "+
				"an epoch manifest (…/v1/epochs/latest/manifest), not at the snapshot", uri)
	}
	return common.HexToHash(m.IndexFilter.Keccak256), nil
}

func loadFilter(ctx context.Context, source string) (*hintfilter.Filter, []byte, error) {
	var raw []byte
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, nil, err
		}
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("fetch %s: %s", source, resp.Status)
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, 256<<20)); err != nil {
			return nil, nil, err
		}
	} else {
		var err error
		if raw, err = os.ReadFile(source); err != nil {
			return nil, nil, err
		}
	}
	f, err := hintfilter.Decode(raw)
	if err != nil {
		return nil, nil, err
	}
	return f, raw, nil
}

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
