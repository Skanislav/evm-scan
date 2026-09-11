// Command evmscan-hint builds and inspects .xorf hint filters.
//
// A hint filter says which contracts are worth reading, in about a byte per entry,
// in a file that can be published and checked. Two kinds get built here:
//
//   - a token filter, over a published tokenlists.org document, which answers "is
//     this contract a known token?" and is how spam gets dropped from a candidate
//     set; and
//   - an index filter, over the (account, token) pairs this deployment has indexed,
//     which answers "did this account touch this contract?" and is how a reader
//     narrows a portfolio read without telling anyone whose portfolio it is.
//
// Both are deterministic: the same input always produces the same bytes, so a
// published filter can be rebuilt by anyone who wants to check that it says what it
// claims. The manifest written alongside carries both digests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
	"github.com/Skanislav/evm-scan/internal/snapshot"
	"github.com/Skanislav/evm-scan/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "build":
		err = build(ctx, os.Args[2:])
	case "test":
		err = testKey(os.Args[2:])
	case "inspect":
		err = inspect(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "evmscan-hint: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `evmscan-hint builds and inspects .xorf hint filters.

  evmscan-hint build -tokenlist <url|path> -chain <id> -o tokens.xorf
  evmscan-hint build -index -dsn <dsn> -chain <id> [-shard] -o index.xorf
  evmscan-hint build -index -snapshot <url|path> -o index.xorf
  evmscan-hint test -f <file> <token> [account]
  evmscan-hint inspect -f <file>

The database DSN also comes from EVMSCAN_DATABASE_DSN.
`)
}

func build(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	list := fs.String("tokenlist", "", "tokenlists.org document, as a URL or a path")
	fromIndex := fs.Bool("index", false, "build over the indexed (account, token) pairs")
	fromSnap := fs.String("snapshot", "", "build the index filter from a published snapshot (path or URL) instead of a database")
	dsn := fs.String("dsn", os.Getenv("EVMSCAN_DATABASE_DSN"), "PostgreSQL DSN (or EVMSCAN_DATABASE_DSN)")
	chainID := fs.Uint64("chain", 1, "chain id")
	toBlock := fs.Uint64("to-block", 0, "index filters only: build as of this block (0 = everything indexed)")
	epochID := fs.Int64("epoch", -1, "index filters only: the epoch this filter corresponds to")
	shard := fs.Bool("shard", false, "index filters only: write 256 files sharded by the account's first byte")
	out := fs.String("o", "", "output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-o is required")
	}
	if (*list == "") == !*fromIndex {
		return fmt.Errorf("give exactly one of -tokenlist or -index")
	}

	if *list != "" {
		return buildTokens(ctx, *list, *chainID, *out)
	}
	if *fromSnap != "" {
		return buildIndexFromSnapshot(ctx, *fromSnap, *out)
	}
	return buildIndex(ctx, *dsn, *chainID, *toBlock, *epochID, *shard, *out)
}

func buildTokens(ctx context.Context, source string, chainID uint64, out string) error {
	tl, err := hintfilter.FetchTokenList(ctx, source)
	if err != nil {
		return err
	}
	addrs := tl.Addresses(chainID)
	if len(addrs) == 0 {
		return fmt.Errorf("%q has no tokens for chain %d", tl.Name, chainID)
	}

	// A token filter is public on purpose. The list it comes from is already
	// published, so there is nothing to hide and everything to gain from letting
	// anyone rebuild the file and diff it.
	sub := hintfilter.Subkey(hintfilter.PublicSecret, chainID, hintfilter.KindToken)
	keys := make([]uint64, len(addrs))
	for i, a := range addrs {
		keys[i] = hintfilter.TokenKey(sub, a)
	}

	f, err := hintfilter.Build(keys, hintfilter.Meta{
		ChainID: chainID, Kind: hintfilter.KindToken, EpochID: -1,
	})
	if err != nil {
		return err
	}
	return write(f, out, fmt.Sprintf("%s (%s)", tl.Name, source), tl.Version.String())
}

func buildIndex(ctx context.Context, dsn string, chainID, toBlock uint64, epochID int64, shard bool, out string) error {
	if dsn == "" {
		return fmt.Errorf("-dsn or EVMSCAN_DATABASE_DSN is required for -index")
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	if toBlock == 0 {
		// Not ^uint64(0): SnapshotIndex casts its argument to int64 for the
		// BIGINT bind, so that sentinel arrives as -1 and quietly matches no rows
		// at all. CoverageRange is the same bound Publisher.Build uses, which also
		// makes it the honest answer to "as of when" — a value the filter has to
		// carry regardless, because a reader shown a portfolio as of an unstated
		// block cannot tell "you hold nothing" from "nothing was indexed yet".
		if _, toBlock, err = st.CoverageRange(ctx, chainID); err != nil {
			return err
		}
	}
	sets, err := st.SnapshotIndex(ctx, chainID, toBlock)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return fmt.Errorf("chain %d has nothing indexed as of block %d", chainID, toBlock)
	}

	sub := hintfilter.Subkey(hintfilter.PublicSecret, chainID, hintfilter.KindAccountToken)

	if !shard {
		var keys []uint64
		for _, s := range sets {
			for _, a := range s.Assets {
				keys = append(keys, hintfilter.PairKey(sub, s.Account, a))
			}
		}
		f, err := hintfilter.Build(keys, hintfilter.Meta{
			ChainID: chainID, Kind: hintfilter.KindAccountToken,
			EpochID: epochID, ToBlock: toBlock,
		})
		if err != nil {
			return err
		}
		return write(f, out, fmt.Sprintf("index chain %d", chainID), "")
	}

	// Sharding trades a little privacy for a lot of bandwidth: a reader fetches one
	// of 256 files instead of the whole index, and reveals the first byte of the
	// address they are asking about. That is the same deal HIBP's range API makes,
	// and at index sizes worth sharding it is the right one — but it is a deal, so
	// it stays opt-in and the UI has to say what it costs.
	buckets := make([][]uint64, 256)
	for _, s := range sets {
		for _, a := range s.Assets {
			b := s.Account[0]
			buckets[b] = append(buckets[b], hintfilter.PairKey(sub, s.Account, a))
		}
	}
	dir := strings.TrimSuffix(out, filepath.Ext(out))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	written := 0
	for i, keys := range buckets {
		if len(keys) == 0 {
			continue
		}
		f, err := hintfilter.Build(keys, hintfilter.Meta{
			ChainID: chainID, Kind: hintfilter.KindAccountToken,
			EpochID: epochID, ToBlock: toBlock,
		})
		if err != nil {
			return fmt.Errorf("shard %02x: %w", i, err)
		}
		p := filepath.Join(dir, fmt.Sprintf("%02x.xorf", i))
		if err := write(f, p, fmt.Sprintf("index chain %d shard %02x", chainID, i), ""); err != nil {
			return err
		}
		written++
	}
	fmt.Printf("wrote %d shards to %s/\n", written, dir)
	return nil
}

// buildIndexFromSnapshot builds the index filter out of a published snapshot.
//
// A snapshot already carries exactly what the filter is over — one leaf per account
// with the contracts it touched — and it names the chain, the block and the epoch it
// was taken at. So the filter can be rebuilt by anyone holding the published table,
// with no database and no node, which is the point: a reader who wants to check that
// a deployment is serving the filter it committed should not have to run that
// deployment to find out.
//
// The digest printed here is the one to compare against the epoch's manifest. If
// they differ, either the snapshot or the served filter is not what was committed.
func buildIndexFromSnapshot(ctx context.Context, source, out string) error {
	rc, err := snapshot.Open(ctx, source)
	if err != nil {
		return err
	}
	defer rc.Close()

	h, _, leaves, err := snapshot.Read(rc)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	if len(leaves) == 0 {
		return fmt.Errorf("%s has no leaves", source)
	}

	sets := make([]hintfilter.AccountAssetSet, len(leaves))
	for i, l := range leaves {
		sets[i] = hintfilter.AccountAssetSet{Account: l.Account, Assets: l.Assets}
	}

	f, enc, m, err := hintfilter.FromIndex(h.ChainID, sets, h.ToBlock)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, enc, 0o644); err != nil {
		return err
	}

	pairs := 0
	for _, l := range leaves {
		pairs += len(l.Assets)
	}
	fmt.Printf("%s: %d accounts, %d pairs, %s, %d bytes (%.2f bytes/key)\n",
		out, len(leaves), pairs, m.Structure, len(enc), float64(len(enc))/float64(f.Count()))
	fmt.Printf("  chain %d as of block %d (snapshot root %s)\n", h.ChainID, h.ToBlock, h.Root)
	fmt.Printf("  keccak256 %s\n", m.Keccak256)
	if h.OnchainEpochID != nil {
		fmt.Printf("  compare against the manifest for on-chain epoch %d\n", *h.OnchainEpochID)
	}
	return writeManifest(m, out)
}

func write(f *hintfilter.Filter, path, source, sourceVersion string) error {
	enc, err := f.Encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, enc, 0o644); err != nil {
		return err
	}

	m := hintfilter.BuildManifest(f, enc, source, sourceVersion)
	if err := writeManifest(m, path); err != nil {
		return err
	}

	fmt.Printf("%s: %d keys, %s, %d bytes (%.2f bytes/key)\n",
		path, m.Count, m.Structure, m.Bytes, float64(m.Bytes)/float64(m.Count))
	fmt.Printf("  keccak256 %s\n", m.Keccak256)
	if m.ToBlock > 0 {
		fmt.Printf("  as of block %d\n", m.ToBlock)
	}
	return nil
}

func writeManifest(m hintfilter.Manifest, path string) error {
	mj, err := m.JSON()
	if err != nil {
		return err
	}
	return os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".manifest.json", mj, 0o644)
}

func testKey(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	path := fs.String("f", "", "filter file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if *path == "" || len(rest) == 0 {
		return fmt.Errorf("usage: evmscan-hint test -f <file> <token> [account]")
	}

	f, err := load(*path)
	if err != nil {
		return err
	}
	if f.Blinded {
		return fmt.Errorf("%s is blinded; only the holder of its secret can test it", *path)
	}

	sub := hintfilter.Subkey(hintfilter.PublicSecret, f.ChainID, f.Kind)
	var key uint64
	switch f.Kind {
	case hintfilter.KindToken:
		if !common.IsHexAddress(rest[0]) {
			return fmt.Errorf("%q is not an address", rest[0])
		}
		key = hintfilter.TokenKey(sub, common.HexToAddress(rest[0]))
	case hintfilter.KindAccountToken:
		if len(rest) < 2 {
			return fmt.Errorf("this filter keys (account, token) pairs: give both")
		}
		if !common.IsHexAddress(rest[0]) || !common.IsHexAddress(rest[1]) {
			return fmt.Errorf("both arguments must be addresses")
		}
		key = hintfilter.PairKey(sub, common.HexToAddress(rest[1]), common.HexToAddress(rest[0]))
	default:
		return fmt.Errorf("unknown key kind %d", f.Kind)
	}

	if f.Contains(key) {
		// Say what a hit actually means. A filter is a reason to go look, never an
		// answer, and a caller who forgets that will report false positives as
		// holdings.
		fmt.Println("present (subject to the filter's false positive rate; confirm with a live read)")
		return nil
	}
	fmt.Println("absent (decisive: filters have no false negatives)")
	os.Exit(1)
	return nil
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	path := fs.String("f", "", "filter file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-f is required")
	}

	raw, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	f, err := hintfilter.Decode(raw)
	if err != nil {
		return err
	}

	m := hintfilter.BuildManifest(f, raw, *path, "")
	m.BuiltAt = ""
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(mj))

	d, err := f.Descriptor()
	if err != nil {
		return err
	}
	if d != nil {
		dj, _ := json.MarshalIndent(d, "", "  ")
		fmt.Printf("salt descriptor:\n%s\n", dj)
	}

	// Check the digest against any manifest sitting next to the file, so a
	// tampered or truncated download is caught here rather than as a portfolio
	// that quietly shows too little.
	mp := strings.TrimSuffix(*path, filepath.Ext(*path)) + ".manifest.json"
	if b, err := os.ReadFile(mp); err == nil {
		var want hintfilter.Manifest
		if json.Unmarshal(b, &want) == nil && want.Keccak256 != "" {
			if want.Keccak256 == m.Keccak256 {
				fmt.Printf("\nmanifest digest matches\n")
			} else {
				return fmt.Errorf("digest mismatch: file is %s, %s says %s", m.Keccak256, mp, want.Keccak256)
			}
		}
	}
	return nil
}

func load(path string) (*hintfilter.Filter, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return hintfilter.FetchFilter(ctx, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return hintfilter.Decode(raw)
}
