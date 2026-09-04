package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Asset lifecycle states.
const (
	StatusPending  = "pending"  // registered, nothing scanned yet
	StatusScanning = "scanning" // backfill in progress
	StatusLive     = "live"     // backfill complete, following the head
	StatusRevoked  = "revoked"  // hint withdrawn on-chain
)

// Asset hint sources.
const (
	SourceOnchain = "onchain" // mirrored from HintRegistry
	SourceLocal   = "local"   // added directly via the API
)

// Asset is a registered contract that we are willing to scan.
type Asset struct {
	ChainID       uint64
	Address       common.Address
	Standard      uint8
	Symbol        string
	Name          string
	Decimals      *int16
	HintFromBlock uint64
	Registrant    *common.Address
	RegistryKey   []byte
	Source        string
	Status        string
}

// Cursor is an asset's two-pointer scan progress.
type Cursor struct {
	ChainID      uint64
	Address      common.Address
	AnchorBlock  uint64
	BackfillNext uint64
	BackfillDone bool
	TailBlock    uint64
	LogsSeen     uint64
}

// UpsertChain records a chain we are indexing.
func (s *Store) UpsertChain(ctx context.Context, chainID uint64, name string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO chains (chain_id, name) VALUES ($1, $2)
		ON CONFLICT (chain_id) DO UPDATE SET name = EXCLUDED.name, updated_at = now()`,
		int64(chainID), name)
	return err
}

// SetChainHead records the latest observed head, for the status endpoint.
func (s *Store) SetChainHead(ctx context.Context, chainID, block uint64, hash common.Hash) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE chains SET head_block = $2, head_hash = $3, updated_at = now() WHERE chain_id = $1`,
		int64(chainID), int64(block), hash.Bytes())
	return err
}

// RegisterAsset inserts an asset hint and seeds its cursor in one transaction.
//
// anchorBlock is the head at registration time: the tail scanner runs forward from
// it so the asset produces useful data immediately, while the backfill walks down
// toward hintFromBlock for history. Re-registering an existing asset updates its
// metadata but never rewinds scan progress.
func (s *Store) RegisterAsset(ctx context.Context, a Asset, anchorBlock uint64) (bool, error) {
	var created bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var registrant []byte
		if a.Registrant != nil {
			registrant = a.Registrant.Bytes()
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO assets (chain_id, address, standard, symbol, name, decimals,
			                    hint_from_block, registrant, registry_key, source, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (chain_id, address) DO UPDATE SET
				standard     = CASE WHEN assets.standard = 0 THEN EXCLUDED.standard ELSE assets.standard END,
				symbol       = COALESCE(NULLIF(EXCLUDED.symbol, ''), assets.symbol),
				name         = COALESCE(NULLIF(EXCLUDED.name, ''), assets.name),
				decimals     = COALESCE(EXCLUDED.decimals, assets.decimals),
				registry_key = COALESCE(EXCLUDED.registry_key, assets.registry_key),
				source       = EXCLUDED.source,
				status       = CASE WHEN assets.status = 'revoked' THEN 'pending' ELSE assets.status END`,
			int64(a.ChainID), a.Address.Bytes(), int16(a.Standard), a.Symbol, a.Name, a.Decimals,
			int64(a.HintFromBlock), registrant, a.RegistryKey, a.Source, StatusPending)
		if err != nil {
			return err
		}
		_ = tag

		// Seed the cursor only on first registration; DO NOTHING protects progress.
		ct, err := tx.Exec(ctx, `
			INSERT INTO asset_cursors (chain_id, address, anchor_block, backfill_next, tail_block)
			VALUES ($1, $2, $3, $4, $3)
			ON CONFLICT (chain_id, address) DO NOTHING`,
			int64(a.ChainID), a.Address.Bytes(), int64(anchorBlock), int64(backfillStart(anchorBlock)))
		if err != nil {
			return err
		}
		created = ct.RowsAffected() > 0
		return nil
	})
	return created, err
}

// backfillStart is the first block the backward scan should examine.
func backfillStart(anchor uint64) uint64 {
	if anchor == 0 {
		return 0
	}
	return anchor - 1
}

// GetAsset loads a single asset hint.
func (s *Store) GetAsset(ctx context.Context, chainID uint64, addr common.Address) (Asset, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT chain_id, address, standard, COALESCE(symbol,''), COALESCE(name,''), decimals,
		       hint_from_block, registrant, registry_key, source, status
		FROM assets WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes())
	a, err := scanAsset(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// ListAssets returns asset hints for a chain, or all chains when chainID is 0.
func (s *Store) ListAssets(ctx context.Context, chainID uint64, includeRevoked bool) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chain_id, address, standard, COALESCE(symbol,''), COALESCE(name,''), decimals,
		       hint_from_block, registrant, registry_key, source, status
		FROM assets
		WHERE ($1 = 0 OR chain_id = $1)
		  AND ($2 OR status <> 'revoked')
		ORDER BY chain_id, address`,
		int64(chainID), includeRevoked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanAsset(r scannable) (Asset, error) {
	var (
		a          Asset
		chainID    int64
		addr       []byte
		standard   int16
		hintFrom   int64
		registrant []byte
	)
	if err := r.Scan(&chainID, &addr, &standard, &a.Symbol, &a.Name, &a.Decimals,
		&hintFrom, &registrant, &a.RegistryKey, &a.Source, &a.Status); err != nil {
		return Asset{}, err
	}
	a.ChainID = uint64(chainID)
	a.Address = common.BytesToAddress(addr)
	a.Standard = uint8(standard)
	a.HintFromBlock = uint64(hintFrom)
	if len(registrant) == common.AddressLength {
		x := common.BytesToAddress(registrant)
		a.Registrant = &x
	}
	return a, nil
}

// SetAssetStatus moves an asset through its lifecycle.
func (s *Store) SetAssetStatus(ctx context.Context, chainID uint64, addr common.Address, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE assets SET status = $3 WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes(), status)
	return err
}

// SetAssetMetadata records what we learned about a contract from probing it.
func (s *Store) SetAssetMetadata(ctx context.Context, chainID uint64, addr common.Address,
	standard uint8, symbol, name string, decimals *int16) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE assets SET
			standard = CASE WHEN $3 = 0 THEN standard ELSE $3 END,
			symbol   = COALESCE(NULLIF($4, ''), symbol),
			name     = COALESCE(NULLIF($5, ''), name),
			decimals = COALESCE($6, decimals)
		WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes(), int16(standard), symbol, name, decimals)
	return err
}

// GetCursor loads scan progress for an asset.
func (s *Store) GetCursor(ctx context.Context, chainID uint64, addr common.Address) (Cursor, error) {
	var (
		c        Cursor
		cid      int64
		a        []byte
		anchor   int64
		next     int64
		tail     int64
		logsSeen int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT chain_id, address, anchor_block, backfill_next, backfill_done, tail_block, logs_seen
		FROM asset_cursors WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes()).
		Scan(&cid, &a, &anchor, &next, &c.BackfillDone, &tail, &logsSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cursor{}, ErrNotFound
	}
	if err != nil {
		return Cursor{}, err
	}
	c.ChainID = uint64(cid)
	c.Address = common.BytesToAddress(a)
	c.AnchorBlock = uint64(anchor)
	c.BackfillNext = uint64(next)
	c.TailBlock = uint64(tail)
	c.LogsSeen = uint64(logsSeen)
	return c, nil
}

// ListCursors returns scan progress for every non-revoked asset on a chain.
func (s *Store) ListCursors(ctx context.Context, chainID uint64) ([]Cursor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.chain_id, c.address, c.anchor_block, c.backfill_next, c.backfill_done,
		       c.tail_block, c.logs_seen
		FROM asset_cursors c
		JOIN assets a ON a.chain_id = c.chain_id AND a.address = c.address
		WHERE c.chain_id = $1 AND a.status <> 'revoked'
		ORDER BY c.address`, int64(chainID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Cursor
	for rows.Next() {
		var (
			c                            Cursor
			cid                          int64
			a                            []byte
			anchor, next, tail, logsSeen int64
		)
		if err := rows.Scan(&cid, &a, &anchor, &next, &c.BackfillDone, &tail, &logsSeen); err != nil {
			return nil, err
		}
		c.ChainID = uint64(cid)
		c.Address = common.BytesToAddress(a)
		c.AnchorBlock = uint64(anchor)
		c.BackfillNext = uint64(next)
		c.TailBlock = uint64(tail)
		c.LogsSeen = uint64(logsSeen)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AdvanceBackfill records downward progress. done marks the history scan complete.
func (s *Store) AdvanceBackfill(ctx context.Context, chainID uint64, addr common.Address,
	next uint64, done bool, logsSeen uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE asset_cursors
		SET backfill_next = $3, backfill_done = $4, logs_seen = logs_seen + $5, updated_at = now()
		WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes(), int64(next), done, int64(logsSeen))
	return err
}

// AdvanceTail records forward progress.
func (s *Store) AdvanceTail(ctx context.Context, chainID uint64, addr common.Address,
	tail uint64, logsSeen uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE asset_cursors
		SET tail_block = GREATEST(tail_block, $3), logs_seen = logs_seen + $4, updated_at = now()
		WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes(), int64(tail), int64(logsSeen))
	return err
}

// RewindTail moves the forward cursor back, so a reorged range is rescanned.
func (s *Store) RewindTail(ctx context.Context, chainID uint64, to uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE asset_cursors SET tail_block = LEAST(tail_block, $2), updated_at = now()
		WHERE chain_id = $1`, int64(chainID), int64(to))
	if err != nil {
		return fmt.Errorf("store: rewind tail: %w", err)
	}
	return nil
}
