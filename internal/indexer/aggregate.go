// Package indexer turns an allowlist of contracts into a per-account discovery index.
//
// Two workers cooperate per chain:
//
//	Backfiller — walks *down* from the block at which an asset was registered toward
//	             its deploy block, giving the "every event at least once" guarantee
//	             that makes the index complete.
//	Follower   — walks *up* from that same anchor, keeping the index current.
//
// Splitting at the registration anchor means a newly registered contract produces
// useful data within one block instead of after a full history scan.
package indexer

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/store"
)

type aggKey struct {
	account common.Address
	asset   common.Address
}

type eventRef struct {
	block    uint64
	logIndex uint
}

// Aggregate folds raw logs into rollup rows.
//
// event_count counts distinct logs rather than distinct participants: a Transfer names
// two accounts, and each should see that as one event on their own row, not two.
func Aggregate(logs []types.Log) []store.Interaction {
	type acc struct {
		row  store.Interaction
		seen map[eventRef]struct{}
	}

	byKey := make(map[aggKey]*acc)

	for i := range logs {
		l := &logs[i]
		if l.Removed {
			continue
		}
		d, ok := evmlog.Decode(l)
		if !ok {
			continue
		}
		for _, p := range d.Participants {
			k := aggKey{account: p.Address, asset: l.Address}
			a := byKey[k]
			if a == nil {
				a = &acc{
					row: store.Interaction{
						Account:    p.Address,
						Asset:      l.Address,
						FirstBlock: l.BlockNumber,
						LastBlock:  l.BlockNumber,
					},
					seen: make(map[eventRef]struct{}),
				}
				byKey[k] = a
			}
			if l.BlockNumber < a.row.FirstBlock {
				a.row.FirstBlock = l.BlockNumber
			}
			if l.BlockNumber > a.row.LastBlock {
				a.row.LastBlock = l.BlockNumber
			}
			a.row.Roles |= roleBit(p.Role)
			a.seen[eventRef{block: l.BlockNumber, logIndex: l.Index}] = struct{}{}
		}
	}

	out := make([]store.Interaction, 0, len(byKey))
	for _, a := range byKey {
		a.row.EventCount = uint64(len(a.seen))
		out = append(out, a.row)
	}
	return out
}

// PendingFrom projects logs into unconfirmed event rows.
func PendingFrom(logs []types.Log) []store.PendingEvent {
	var out []store.PendingEvent
	for i := range logs {
		l := &logs[i]
		if l.Removed {
			continue
		}
		d, ok := evmlog.Decode(l)
		if !ok {
			continue
		}
		for _, p := range d.Participants {
			out = append(out, store.PendingEvent{
				BlockNumber: l.BlockNumber,
				BlockHash:   l.BlockHash,
				LogIndex:    uint32(l.Index),
				Asset:       l.Address,
				Account:     p.Address,
				Role:        uint8(p.Role),
			})
		}
	}
	return out
}

// roleBit maps a role to its bit in the interactions.roles mask.
func roleBit(r evmlog.Role) uint32 {
	if r == 0 {
		return 0
	}
	return 1 << (uint32(r) - 1)
}

// RoleNames expands a roles bitmask for API responses.
func RoleNames(mask uint32) []string {
	all := []evmlog.Role{
		evmlog.RoleSender, evmlog.RoleReceiver, evmlog.RoleOwner,
		evmlog.RoleSpender, evmlog.RoleOperator,
	}
	var out []string
	for _, r := range all {
		if mask&roleBit(r) != 0 {
			out = append(out, r.String())
		}
	}
	return out
}

// ObservedStandards tallies the token standards a contract's logs look like, so a
// registration that did not declare a kind can be classified from evidence.
func ObservedStandards(logs []types.Log) map[evmlog.Standard]int {
	seen := map[evmlog.Standard]int{}
	for i := range logs {
		if d, ok := evmlog.Decode(&logs[i]); ok && d.Standard != evmlog.StandardUnknown {
			seen[d.Standard]++
		}
	}
	return seen
}
