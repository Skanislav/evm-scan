package snapshot

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/store"
)

// RestoreStore is the slice of the store a restore writes through. It is an
// interface so the round-trip test can drive it without Postgres.
type RestoreStore interface {
	UpsertChain(ctx context.Context, chainID uint64, name string) error
	RegisterAsset(ctx context.Context, a store.Asset, anchorBlock uint64) (bool, error)
	FoldInteractions(ctx context.Context, chainID uint64, rows []store.Interaction) error
	AdvanceBackfill(ctx context.Context, chainID uint64, addr common.Address, next uint64, done bool, logsSeen uint64) error
	AdvanceTail(ctx context.Context, chainID uint64, addr common.Address, tail uint64, logsSeen uint64) error
	SetBackfillFloor(ctx context.Context, chainID uint64, addr common.Address, floor uint64) error
}

// RestoreReport says what a restore wrote.
type RestoreReport struct {
	ChainID      uint64
	Assets       int
	Accounts     int
	Interactions int
	FromBlock    uint64
	ToBlock      uint64
}

// Restore rebuilds an index from a verified snapshot.
//
// Call it only on a snapshot whose roots matched the registry — this writes
// whatever it is handed, and the roots are the only thing that makes the document
// worth writing. evmscan-verify -snapshot is that check.
//
// It restores what the commitment actually covers, which is *not* everything the
// original database held:
//
//   - account → assets comes back exactly, because that is what the root commits.
//   - per-asset scan ranges come back exactly, from the coverage leaves, so a
//     restarted daemon resumes at the right blocks instead of rewalking history it
//     could no longer reach anyway.
//   - first_block, last_block, event_count and roles do NOT come back. No merkle
//     leaf commits them, so a snapshot cannot prove them and this refuses to invent
//     them: the range is recorded as the epoch's own, and counts stay zero until the
//     follower observes real events.
//
// That last point is the honest boundary of this design. A restored node knows
// which contracts an account touched and can prove it against the chain; it does
// not know how many times.
func Restore(ctx context.Context, st RestoreStore, h Header, coverage []Coverage, leaves []Leaf) (RestoreReport, error) {
	rep := RestoreReport{ChainID: h.ChainID, FromBlock: h.FromBlock, ToBlock: h.ToBlock}

	if err := st.UpsertChain(ctx, h.ChainID, fmt.Sprintf("chain-%d", h.ChainID)); err != nil {
		return rep, fmt.Errorf("upsert chain: %w", err)
	}

	// Assets first: interactions reference them, and the cursors below are what stop
	// a restored follower from walking back to genesis.
	for _, c := range coverage {
		a := store.Asset{
			ChainID:       h.ChainID,
			Address:       c.Asset,
			HintFromBlock: c.FromBlock,
			Source:        "snapshot",
			Status:        "active",
			RegistryKey:   AssetKey(h.ChainID, c.Asset).Bytes(),
		}
		if _, err := st.RegisterAsset(ctx, a, c.FromBlock); err != nil {
			return rep, fmt.Errorf("restore asset %s: %w", c.Asset.Hex(), err)
		}
		// The publisher stood behind [FromBlock, ToBlock] for this asset, so that is
		// exactly the range to declare scanned: backfill complete down to FromBlock,
		// tail already at ToBlock. Anything else makes the node re-walk blocks a light
		// client can no longer serve, and report a coverage range it cannot honour.
		// backfill_next is the highest block not yet scanned, walking down, so a
		// completed walk sits one below the range's floor. logs_seen is a counter of
		// work done and is left at zero: no logs were read to produce these rows.
		next := uint64(0)
		if c.FromBlock > 0 {
			next = c.FromBlock - 1
		}
		if err := st.AdvanceBackfill(ctx, h.ChainID, c.Asset, next, true, 0); err != nil {
			return rep, fmt.Errorf("restore backfill cursor %s: %w", c.Asset.Hex(), err)
		}
		if err := st.SetBackfillFloor(ctx, h.ChainID, c.Asset, c.FromBlock); err != nil {
			return rep, fmt.Errorf("restore backfill floor %s: %w", c.Asset.Hex(), err)
		}
		if err := st.AdvanceTail(ctx, h.ChainID, c.Asset, c.ToBlock, 0); err != nil {
			return rep, fmt.Errorf("restore tail cursor %s: %w", c.Asset.Hex(), err)
		}
		rep.Assets++
	}

	// Interactions in batches: a real epoch is hundreds of thousands of accounts and
	// several times that in rows, which is not one statement.
	const batch = 2000
	rows := make([]store.Interaction, 0, batch)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		if err := st.FoldInteractions(ctx, h.ChainID, rows); err != nil {
			return fmt.Errorf("restore interactions: %w", err)
		}
		rows = rows[:0]
		return nil
	}
	for _, l := range leaves {
		for _, asset := range l.Assets {
			rows = append(rows, store.Interaction{
				Account: l.Account,
				Asset:   asset,
				// Not observed, declared: the commitment says this account touched this
				// asset somewhere inside the epoch's range, and nothing narrower.
				FirstBlock: h.FromBlock,
				LastBlock:  h.ToBlock,
				EventCount: 0,
				Roles:      0,
			})
			rep.Interactions++
			if len(rows) >= batch {
				if err := flush(); err != nil {
					return rep, err
				}
			}
		}
		rep.Accounts++
	}
	if err := flush(); err != nil {
		return rep, err
	}
	return rep, nil
}
