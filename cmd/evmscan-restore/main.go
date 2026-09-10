// Command evmscan-restore rebuilds an index from a published snapshot.
//
// The recovery story this completes: HintRegistry commits a merkle root, a root
// proves a leaf to whoever already has it and recovers nothing, so an index that
// lives only in the publisher's Postgres dies with that database. Epoch.uri points
// at the table instead, IndexPublished emits the pointer, and this reads it back.
//
// The snapshot is checked against the chain before a single row is written. That is
// the point of the design rather than a precaution: the file may come from any
// mirror, and the roots on chain are what make it safe to believe. -skip-verify
// exists for a file already verified in a previous step, and says so loudly.
//
//	evmscan-restore -snapshot https://host/v1/epochs/2/snapshot \
//	  -node https://base-rpc -registry 0x… \
//	  -dsn postgres://…
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/snapshot"
	"github.com/Skanislav/evm-scan/internal/store"
)

func main() {
	var (
		src        = flag.String("snapshot", "", "snapshot to restore: path, URL, or - for stdin")
		dsn        = flag.String("dsn", os.Getenv("EVMSCAN_DATABASE_DSN"), "PostgreSQL DSN (default $EVMSCAN_DATABASE_DSN)")
		nodeURL    = flag.String("node", "", "RPC for the chain the registry is on, to check the roots")
		registry   = flag.String("registry", os.Getenv("EVMSCAN_REGISTRY_ADDRESS"), "HintRegistry address")
		epoch      = flag.Int64("epoch", -1, "on-chain epoch id (default: the one named in the snapshot)")
		skipVerify = flag.Bool("skip-verify", false, "restore without checking the snapshot against the chain")
		dryRun     = flag.Bool("dry-run", false, "verify and report, write nothing")
	)
	flag.Parse()

	if *src == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*src, *dsn, *nodeURL, *registry, *epoch, *skipVerify, *dryRun); err != nil {
		log.Fatal(err)
	}
}

func run(src, dsn, nodeURL, registryAddr string, epochID int64, skipVerify, dryRun bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	rc, err := snapshot.Open(ctx, src)
	if err != nil {
		return err
	}
	defer rc.Close()

	fmt.Printf("reading %s\n", src)
	h, coverage, leaves, err := snapshot.Read(rc)
	if err != nil {
		return err
	}
	fmt.Printf("  chain %d, blocks %d..%d — %d accounts over %d assets\n",
		h.ChainID, h.FromBlock, h.ToBlock, len(leaves), len(coverage))

	if err := verify(ctx, h, coverage, leaves, nodeURL, registryAddr, epochID, skipVerify); err != nil {
		return err
	}

	if dryRun {
		fmt.Println("\n-dry-run: nothing written.")
		return nil
	}
	if dsn == "" {
		return fmt.Errorf("-dsn is required to write (or pass -dry-run)")
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	fmt.Println("\nrestoring…")
	start := time.Now()
	rep, err := snapshot.Restore(ctx, st, h, coverage, leaves)
	if err != nil {
		return err
	}
	fmt.Printf("restored %d assets, %d accounts, %d interactions in %s\n",
		rep.Assets, rep.Accounts, rep.Interactions, time.Since(start).Round(time.Second))
	fmt.Println("\nEvent counts and per-asset block ranges are not part of the commitment, so they")
	fmt.Println("did not come back: rows carry the epoch's range and a zero count until the")
	fmt.Println("follower observes real events. Which contracts an account touched is exact.")
	return nil
}

// verify refuses the restore unless the snapshot rebuilds the roots the registry
// holds. A snapshot is only trustworthy because of this check; without it the file
// is an unauthenticated download that gets written straight into the index.
func verify(ctx context.Context, h snapshot.Header, coverage []snapshot.Coverage, leaves []snapshot.Leaf,
	nodeURL, registryAddr string, epochID int64, skip bool) error {

	if skip {
		fmt.Println("\n!! -skip-verify: writing this snapshot without checking it against the chain.")
		fmt.Println("!! Only do this for a file already verified with evmscan-verify -snapshot.")
		return nil
	}
	if nodeURL == "" || registryAddr == "" {
		return fmt.Errorf("-node and -registry are required to check the snapshot " +
			"(or pass -skip-verify, having checked it another way)")
	}
	if !common.IsHexAddress(registryAddr) {
		return fmt.Errorf("%q is not an address", registryAddr)
	}

	// Rebuilt from the rows just read, not from the header, so an edited document
	// cannot pass by restating what it wants to be true.
	res := snapshot.Rebuild(h, coverage, leaves)

	if epochID < 0 {
		if h.OnchainEpochID == nil {
			return fmt.Errorf("snapshot names no on-chain epoch; pass -epoch")
		}
		epochID = *h.OnchainEpochID
	}

	node, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		return err
	}
	defer node.Close()

	client, err := hintreg.NewClient(node, common.HexToAddress(registryAddr))
	if err != nil {
		return err
	}
	on, err := client.GetEpoch(ctx, epochID)
	if err != nil {
		return fmt.Errorf("read on-chain epoch %d: %w", epochID, err)
	}
	if on.ChainID != h.ChainID {
		return fmt.Errorf("epoch %d is about chain %d, snapshot is about chain %d",
			epochID, on.ChainID, h.ChainID)
	}
	if !res.Matches(on.Root, on.CoverageRoot) {
		return fmt.Errorf("snapshot does NOT match epoch %d — refusing to restore\n"+
			"  rebuilt  root %s\n           coverage %s\n"+
			"  on-chain root %s\n           coverage %s",
			epochID, res.Root.Hex(), res.CoverageRoot.Hex(), on.Root.Hex(), on.CoverageRoot.Hex())
	}
	fmt.Printf("  verified against on-chain epoch %d (%s): root %s\n",
		epochID, on.Status, res.Root.Hex())
	if on.Status != hintreg.EpochFinalized {
		fmt.Printf("  note: epoch %d is %s, not finalized — this commitment can still be rejected.\n",
			epochID, on.Status)
	}
	return nil
}
