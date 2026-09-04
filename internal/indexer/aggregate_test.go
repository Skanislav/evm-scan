package indexer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/evmlog"
)

var (
	tokenA = common.HexToAddress("0xAAAAaAAAaAAAAaaaAaaAaaaAAAAAAAaaaAAAaAAa")
	alice  = common.HexToAddress("0x1111111111111111111111111111111111111111")
	bob    = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

func topicOf(a common.Address) common.Hash { return common.BytesToHash(a.Bytes()) }

func transferLog(block uint64, idx uint, from, to common.Address) types.Log {
	return types.Log{
		Address:     tokenA,
		BlockNumber: block,
		Index:       idx,
		Topics:      []common.Hash{evmlog.SigTransfer, topicOf(from), topicOf(to)},
	}
}

// TestAggregateCountsLogsNotParticipants: a Transfer names two accounts, and each
// should see it as one event on their own row rather than two.
func TestAggregateCountsLogsNotParticipants(t *testing.T) {
	rows := Aggregate([]types.Log{transferLog(10, 0, alice, bob)})

	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per participant", len(rows))
	}
	for _, r := range rows {
		if r.EventCount != 1 {
			t.Errorf("%s: event_count = %d, want 1", r.Account.Hex(), r.EventCount)
		}
		if r.Asset != tokenA {
			t.Errorf("asset = %s, want %s", r.Asset.Hex(), tokenA.Hex())
		}
	}
}

func TestAggregateTracksBlockRangeAndRoles(t *testing.T) {
	rows := Aggregate([]types.Log{
		transferLog(10, 0, alice, bob),
		transferLog(25, 1, bob, alice),
	})

	byAccount := map[common.Address]struct {
		first, last, count uint64
		roles              uint32
	}{}
	for _, r := range rows {
		byAccount[r.Account] = struct {
			first, last, count uint64
			roles              uint32
		}{r.FirstBlock, r.LastBlock, r.EventCount, r.Roles}
	}

	a := byAccount[alice]
	if a.first != 10 || a.last != 25 {
		t.Errorf("alice block range = [%d,%d], want [10,25]", a.first, a.last)
	}
	if a.count != 2 {
		t.Errorf("alice event_count = %d, want 2", a.count)
	}

	// Alice sent once and received once, so both role bits must be set.
	want := roleBit(evmlog.RoleSender) | roleBit(evmlog.RoleReceiver)
	if a.roles != want {
		t.Errorf("alice roles = %b, want %b (sender|receiver)", a.roles, want)
	}
}

// TestAggregateDeduplicatesTheSameLog guards against a re-scanned range inflating
// counts when the same log is presented twice.
func TestAggregateDeduplicatesTheSameLog(t *testing.T) {
	l := transferLog(10, 0, alice, bob)
	rows := Aggregate([]types.Log{l, l})

	for _, r := range rows {
		if r.EventCount != 1 {
			t.Errorf("%s: event_count = %d, want 1 for a repeated log", r.Account.Hex(), r.EventCount)
		}
	}
}

func TestAggregateSkipsRemovedLogs(t *testing.T) {
	l := transferLog(10, 0, alice, bob)
	l.Removed = true

	if rows := Aggregate([]types.Log{l}); len(rows) != 0 {
		t.Errorf("got %d rows, want 0 for a reorg-removed log", len(rows))
	}
}

func TestAggregateSeparatesAssets(t *testing.T) {
	other := common.HexToAddress("0xBBBBbBbBbbBBbbBbbbbBBBbbBbBBbbbBbbBBBBbB")
	l2 := transferLog(11, 0, alice, bob)
	l2.Address = other

	rows := Aggregate([]types.Log{transferLog(10, 0, alice, bob), l2})

	assets := map[common.Address]int{}
	for _, r := range rows {
		assets[r.Asset]++
	}
	if len(assets) != 2 {
		t.Errorf("got %d distinct assets, want 2", len(assets))
	}
}

func TestPendingFromExpandsParticipants(t *testing.T) {
	evs := PendingFrom([]types.Log{transferLog(10, 7, alice, bob)})

	if len(evs) != 2 {
		t.Fatalf("got %d pending events, want 2", len(evs))
	}
	for _, e := range evs {
		if e.BlockNumber != 10 || e.LogIndex != 7 {
			t.Errorf("event = block %d index %d, want block 10 index 7", e.BlockNumber, e.LogIndex)
		}
		if e.Asset != tokenA {
			t.Errorf("asset = %s, want %s", e.Asset.Hex(), tokenA.Hex())
		}
	}
}

func TestRoleNamesExpandsBitmask(t *testing.T) {
	mask := roleBit(evmlog.RoleSender) | roleBit(evmlog.RoleOperator)
	got := RoleNames(mask)

	want := map[string]bool{"sender": true, "operator": true}
	if len(got) != len(want) {
		t.Fatalf("RoleNames(%b) = %v, want %v", mask, got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected role %q", n)
		}
	}
	if len(RoleNames(0)) != 0 {
		t.Error("an empty mask should expand to no roles")
	}
}

func TestRoleBitsAreDistinct(t *testing.T) {
	// The bitmask is persisted, so collisions would silently merge roles.
	seen := map[uint32]evmlog.Role{}
	for _, r := range []evmlog.Role{
		evmlog.RoleSender, evmlog.RoleReceiver, evmlog.RoleOwner,
		evmlog.RoleSpender, evmlog.RoleOperator,
	} {
		b := roleBit(r)
		if b == 0 {
			t.Errorf("role %v has no bit", r)
		}
		if prev, dup := seen[b]; dup {
			t.Errorf("roles %v and %v share bit %b", prev, r, b)
		}
		seen[b] = r
	}
}
