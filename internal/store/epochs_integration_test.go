package store_test

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// TestPruneEpochData: leaves survive for the latest finalized epoch and everything
// newer; coverage survives until claimed; the epoch rows themselves always survive.
func TestPruneEpochData(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const chain = uint64(910_001) // a chain id nothing else in the scratch DB uses

	mk := func(i int) int64 {
		acct := common.BigToAddress(common.Big1)
		id, err := st.CreateEpoch(ctx, store.Epoch{
			ChainID: chain, FromBlock: 1, ToBlock: uint64(10 + i),
			MerkleRoot: common.HexToHash("0x01"), CoverageRoot: common.HexToHash("0x02"),
			LeafCount: 1, Status: store.EpochBuilt,
		}, []store.EpochLeaf{{Index: 0, Account: acct, AssetsHash: common.HexToHash("0x03"), Leaf: common.HexToHash("0x04")}},
			[]store.EpochCoverage{{Index: 0, Asset: acct, RegistryKey: common.HexToHash("0x05"), FromBlock: 1, ToBlock: 2, Leaf: common.HexToHash("0x06")}})
		if err != nil {
			t.Fatalf("create epoch %d: %v", i, err)
		}
		return id
	}
	leaves := func(id int64) bool {
		ok, err := st.HasEpochLeaves(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	coverage := func(id int64) int {
		c, err := st.EpochCoverage(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return len(c)
	}

	// e1: built, never posted.  e2: published, then finalized.  e3: published.
	// e4: built (the newest; a retry may still post it).
	e1, e2, e3, e4 := mk(1), mk(2), mk(3), mk(4)
	for i, id := range []int64{e2, e3} {
		if err := st.MarkPublished(ctx, id, int64(i), common.HexToHash("0xaa")); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetEpochStatus(ctx, e2, store.EpochFinalized); err != nil {
		t.Fatal(err)
	}

	rep, err := st.PruneEpochData(ctx, chain)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.LeafEpochs != 1 || rep.CoverageEpochs != 1 {
		t.Fatalf("report = %+v, want 1 leaf epoch (e1) and 1 coverage epoch (e1)", rep)
	}
	if leaves(e1) || !leaves(e2) || !leaves(e3) || !leaves(e4) {
		t.Fatalf("leaves after prune: e1=%v e2=%v e3=%v e4=%v", leaves(e1), leaves(e2), leaves(e3), leaves(e4))
	}
	if coverage(e1) != 0 || coverage(e2) != 1 || coverage(e3) != 1 {
		t.Fatalf("coverage after prune: e1=%d e2=%d e3=%d; e2 is finalized but unclaimed", coverage(e1), coverage(e2), coverage(e3))
	}
	for _, id := range []int64{e1, e2, e3, e4} {
		if _, err := st.GetEpoch(ctx, id); err != nil {
			t.Fatalf("epoch row %d should survive: %v", id, err)
		}
	}

	// e3 finalizes and supersedes e2; e2's reward is claimed. Now e2's leaves and
	// coverage both go, e3's stay.
	if err := st.SetEpochStatus(ctx, e3, store.EpochFinalized); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkClaimed(ctx, e2, common.HexToHash("0xbb"), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PruneEpochData(ctx, chain); err != nil {
		t.Fatal(err)
	}
	if leaves(e2) || !leaves(e3) || !leaves(e4) {
		t.Fatalf("after e3 finalized: e2=%v e3=%v e4=%v", leaves(e2), leaves(e3), leaves(e4))
	}
	if coverage(e2) != 0 || coverage(e3) != 1 {
		t.Fatalf("coverage after claim: e2=%d e3=%d", coverage(e2), coverage(e3))
	}

	// Idempotent and quiet once there is nothing to do.
	rep, err = st.PruneEpochData(ctx, chain)
	if err != nil || rep != (store.PruneReport{}) {
		t.Fatalf("second prune = %+v, %v", rep, err)
	}
}
