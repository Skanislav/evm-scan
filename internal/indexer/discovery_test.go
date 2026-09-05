package indexer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/evmlog"
)

func nftTransfer(block uint64, idx uint, contract common.Address, from, to common.Address) types.Log {
	return types.Log{
		Address:     contract,
		BlockNumber: block,
		Index:       idx,
		Topics: []common.Hash{
			evmlog.SigTransfer, topicOf(from), topicOf(to), {0x01},
		},
	}
}

func candidateByAddr(t *testing.T, logs []types.Log, skip map[common.Address]bool, want common.Address) (found bool, ev, blocks uint64, std uint8) {
	t.Helper()
	for _, c := range CandidatesFrom(logs, skip) {
		if c.Address == want {
			return true, c.EventCount, c.BlocksSeen, c.Standard
		}
	}
	return false, 0, 0, 0
}

func TestCandidatesFromCountsEventsAndDistinctBlocks(t *testing.T) {
	logs := []types.Log{
		transferLog(10, 0, alice, bob),
		transferLog(10, 1, bob, alice), // same block
		transferLog(12, 0, alice, bob),
	}

	found, ev, blocks, std := candidateByAddr(t, logs, nil, tokenA)
	if !found {
		t.Fatal("contract was not discovered")
	}
	if ev != 3 {
		t.Errorf("event_count = %d, want 3", ev)
	}
	// Two events landed in block 10; blocks_seen counts blocks, not events, because a
	// burst in one block is what a spam airdrop looks like.
	if blocks != 2 {
		t.Errorf("blocks_seen = %d, want 2", blocks)
	}
	if std != uint8(evmlog.StandardERC20) {
		t.Errorf("standard = %d, want erc20", std)
	}
}

// TestCandidatesFromSkipsIndexedContracts: anything already indexed is covered by the
// tail scanner, so counting it again would only distort the ranking.
func TestCandidatesFromSkipsIndexedContracts(t *testing.T) {
	logs := []types.Log{transferLog(10, 0, alice, bob)}
	skip := map[common.Address]bool{tokenA: true}

	if got := CandidatesFrom(logs, skip); len(got) != 0 {
		t.Errorf("got %d candidates, want 0 for an already-indexed contract", len(got))
	}
}

func TestCandidatesFromSeparatesContracts(t *testing.T) {
	other := common.HexToAddress("0xBBBBbBbBbbBBbbBbbbbBBBbbBbBBbbbBbbBBBBbB")
	logs := []types.Log{
		transferLog(10, 0, alice, bob),
		nftTransfer(11, 0, other, alice, bob),
	}

	got := CandidatesFrom(logs, nil)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}

	_, _, _, stdA := candidateByAddr(t, logs, nil, tokenA)
	_, _, _, stdB := candidateByAddr(t, logs, nil, other)
	if stdA != uint8(evmlog.StandardERC20) {
		t.Errorf("tokenA standard = %d, want erc20", stdA)
	}
	if stdB != uint8(evmlog.StandardERC721) {
		t.Errorf("other standard = %d, want erc721", stdB)
	}
}

func TestCandidatesFromTracksBlockRange(t *testing.T) {
	logs := []types.Log{
		transferLog(50, 0, alice, bob),
		transferLog(10, 0, alice, bob),
		transferLog(30, 0, alice, bob),
	}

	cands := CandidatesFrom(logs, nil)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].FirstSeenBlock != 10 || cands[0].LastSeenBlock != 50 {
		t.Errorf("range = [%d,%d], want [10,50]",
			cands[0].FirstSeenBlock, cands[0].LastSeenBlock)
	}
}

func TestCandidatesFromIgnoresRemovedAndUndecodableLogs(t *testing.T) {
	removed := transferLog(10, 0, alice, bob)
	removed.Removed = true

	unrelated := types.Log{
		Address:     tokenA,
		BlockNumber: 11,
		Topics:      []common.Hash{{0xab}, topicOf(alice), topicOf(bob)},
	}

	if got := CandidatesFrom([]types.Log{removed, unrelated}, nil); len(got) != 0 {
		t.Errorf("got %d candidates, want 0", len(got))
	}
}

func TestDiscoveryOptionsDefaultsAreSane(t *testing.T) {
	o := DiscoveryOptions{}.withDefaults()

	if o.Lookback == 0 || o.MaxBlocksPerTick == 0 || o.Interval <= 0 {
		t.Fatalf("defaults left a zero value: %+v", o)
	}
	if o.MinEvents == 0 || o.MinBlocks == 0 || o.MaxPromotionsPerTick == 0 {
		t.Errorf("promotion thresholds default to zero, which would promote everything: %+v", o)
	}
	// Auto-promotion commits a full backfill per contract, so it must stay opt-in.
	if o.AutoPromote {
		t.Error("AutoPromote defaulted to true; promotion must be opt-in")
	}
	// Discovery itself must also stay opt-in, since it is the one query that looks at
	// every contract on the chain.
	if (DiscoveryOptions{}).Enabled {
		t.Error("discovery defaulted to enabled")
	}
}

// TestResolveBackfillFloorNeverGoesBelowTheNode encodes the rule that replaced
// "walk to genesis": whatever anyone asks for, a backfill stops where the node's
// receipts stop.
func TestResolveBackfillFloorNeverGoesBelowTheNode(t *testing.T) {
	cases := []struct {
		name             string
		requested, floor uint64
		want             uint64
	}{
		{"node has everything", 0, 0, 0},
		{"node pruned below the request", 0, 15_000_000, 15_000_000},
		{"request is already above the horizon", 18_000_000, 15_000_000, 18_000_000},
		{"request equals the horizon", 15_000_000, 15_000_000, 15_000_000},
		{"deploy block above a pruned node", 19_500_000, 15_000_000, 19_500_000},
	}
	for _, tc := range cases {
		if got := resolveBackfillFloor(tc.requested, tc.floor); got != tc.want {
			t.Errorf("%s: resolveBackfillFloor(%d, %d) = %d, want %d",
				tc.name, tc.requested, tc.floor, got, tc.want)
		}
	}
}
