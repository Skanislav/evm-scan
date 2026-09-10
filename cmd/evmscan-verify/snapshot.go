package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintreg"
	"github.com/Skanislav/evm-scan/internal/snapshot"
)

// runSnapshot checks a published index table against what the registry committed.
//
// This is the recovery proof. A root proves a leaf to whoever already holds it and
// recovers nothing on its own, so an index that exists only in the publisher's
// database dies with it. A snapshot is the table itself; this rebuilds both merkle
// trees from that table and compares them with the epoch on chain.
//
// Nothing about the source is trusted. Every leaf is recomputed from the account
// and its assets, and every coverage leaf from the asset key and its range, so a
// document that has been edited anywhere produces a different root. A snapshot that
// matches can therefore be fetched from a stranger, and one that does not is
// refused no matter who served it.
func runSnapshot(ctx context.Context, source, nodeURL, registryAddr string, epochID int64) error {
	rc, err := snapshot.Open(ctx, source)
	if err != nil {
		return err
	}
	defer rc.Close()

	start := time.Now()
	res, err := snapshot.Verify(rc, nil)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}

	fmt.Printf("snapshot %s\n", source)
	fmt.Printf("  format         %s\n", res.Header.Format)
	fmt.Printf("  indexed chain  %d, blocks %d..%d\n", res.Header.ChainID, res.Header.FromBlock, res.Header.ToBlock)
	fmt.Printf("  rebuilt        %d leaves, %d assets in %s\n", res.Leaves, res.Assets, time.Since(start).Round(time.Millisecond))
	fmt.Printf("  root           %s\n", res.Root.Hex())
	fmt.Printf("  coverage root  %s\n", res.CoverageRoot.Hex())

	// The document names the roots it believes it has. That is a self-consistency
	// check and not evidence of anything, so it is reported separately from the
	// on-chain comparison below, which is the one that counts.
	if want := res.Header.Root; want != "" && !strings.EqualFold(want, res.Root.Hex()) {
		return fmt.Errorf("snapshot disagrees with itself: header says root %s, its rows build %s",
			want, res.Root.Hex())
	}
	if want := res.Header.CoverageRoot; want != "" && !strings.EqualFold(want, res.CoverageRoot.Hex()) {
		return fmt.Errorf("snapshot disagrees with itself: header says coverage root %s, its rows build %s",
			want, res.CoverageRoot.Hex())
	}

	if nodeURL == "" || registryAddr == "" {
		fmt.Println("\nno -node/-registry given, so nothing was checked against the chain.")
		fmt.Println("the roots above are only what this file builds; compare them with getEpoch() to mean anything.")
		return nil
	}
	if !common.IsHexAddress(registryAddr) {
		return fmt.Errorf("%q is not an address", registryAddr)
	}

	if epochID < 0 {
		if res.Header.OnchainEpochID == nil {
			return fmt.Errorf("snapshot carries no on-chain epoch id; pass -epoch")
		}
		epochID = *res.Header.OnchainEpochID
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

	fmt.Printf("\non-chain epoch %d (%s)\n", epochID, on.Status)
	fmt.Printf("  root           %s\n", on.Root.Hex())
	fmt.Printf("  coverage root  %s\n", on.CoverageRoot.Hex())
	if on.URI != "" {
		fmt.Printf("  uri            %s\n", on.URI)
	}

	// A snapshot of the right shape for the wrong epoch would otherwise fail on the
	// roots with nothing to say about why.
	if on.ChainID != res.Header.ChainID {
		return fmt.Errorf("epoch %d is about chain %d, snapshot is about chain %d",
			epochID, on.ChainID, res.Header.ChainID)
	}
	if !res.Matches(on.Root, on.CoverageRoot) {
		return fmt.Errorf("snapshot does NOT match epoch %d: this file is not what was committed", epochID)
	}

	fmt.Printf("\nVERIFIED: this table is exactly what epoch %d committed.\n", epochID)
	fmt.Printf("  %d accounts over %d assets, blocks %d..%d on chain %d\n",
		res.Leaves, res.Assets, on.FromBlock, on.ToBlock, on.ChainID)
	if on.Status != hintreg.EpochFinalized {
		fmt.Printf("  note: the epoch is %s, not finalized — the commitment can still change.\n", on.Status)
	}
	return nil
}
