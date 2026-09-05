package store

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Candidate is a contract observed at the head that we have not committed to
// indexing. It carries counters only — never per-account rows — which is what makes
// watching every contract on the chain affordable.
type Candidate struct {
	ChainID         uint64
	Address         common.Address
	Standard        uint8
	FirstSeenBlock  uint64
	LastSeenBlock   uint64
	EventCount      uint64
	BlocksSeen      uint64
	PromotedAt      *time.Time
	PromotionReason string
}

// SetHistoryFloor records the oldest block this chain's node can serve logs for.
func (s *Store) SetHistoryFloor(ctx context.Context, chainID, floor uint64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE chains SET history_floor = $2, history_probed_at = now(), updated_at = now()
		WHERE chain_id = $1`, int64(chainID), int64(floor))
	return err
}

// HistoryFloor returns the recorded floor and whether the node has been probed yet.
func (s *Store) HistoryFloor(ctx context.Context, chainID uint64) (floor uint64, probed bool, err error) {
	var f int64
	var at *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT history_floor, history_probed_at FROM chains WHERE chain_id = $1`,
		int64(chainID)).Scan(&f, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return uint64(f), at != nil, nil
}

// DiscoveryCursor reports how far the head-watching sweep has read.
func (s *Store) DiscoveryCursor(ctx context.Context, chainID uint64) (uint64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT last_block FROM discovery_cursor WHERE chain_id = $1`, int64(chainID)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return uint64(n), err
}

// SetDiscoveryCursor advances the discovery sweep.
func (s *Store) SetDiscoveryCursor(ctx context.Context, chainID, block uint64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO discovery_cursor (chain_id, last_block) VALUES ($1, $2)
		ON CONFLICT (chain_id) DO UPDATE SET last_block = EXCLUDED.last_block, updated_at = now()`,
		int64(chainID), int64(block))
	return err
}

// UpsertCandidates merges observation counters for contracts seen at the head.
//
// Counters accumulate rather than overwrite, so a re-scanned range would double-count.
// The discovery sweep is therefore driven by a monotonic cursor and never replays a
// range it has already folded in.
func (s *Store) UpsertCandidates(ctx context.Context, chainID uint64, rows []Candidate) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
		INSERT INTO candidates
			(chain_id, address, standard, first_seen_block, last_seen_block, event_count, blocks_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (chain_id, address) DO UPDATE SET
			standard         = CASE WHEN candidates.standard = 0 THEN EXCLUDED.standard ELSE candidates.standard END,
			first_seen_block = LEAST(candidates.first_seen_block, EXCLUDED.first_seen_block),
			last_seen_block  = GREATEST(candidates.last_seen_block, EXCLUDED.last_seen_block),
			event_count      = candidates.event_count + EXCLUDED.event_count,
			blocks_seen      = candidates.blocks_seen + EXCLUDED.blocks_seen,
			updated_at       = now()`

	return s.inTx(ctx, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, c := range rows {
			b.Queue(q, int64(chainID), c.Address.Bytes(), int16(c.Standard),
				int64(c.FirstSeenBlock), int64(c.LastSeenBlock),
				int64(c.EventCount), int64(c.BlocksSeen))
		}
		br := tx.SendBatch(ctx, b)
		defer br.Close()
		for range rows {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListCandidates returns observed contracts ranked by activity.
func (s *Store) ListCandidates(ctx context.Context, chainID uint64, includePromoted bool, limit int) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chain_id, address, standard, first_seen_block, last_seen_block,
		       event_count, blocks_seen, promoted_at, COALESCE(promotion_reason, '')
		FROM candidates
		WHERE chain_id = $1 AND ($2 OR promoted_at IS NULL)
		ORDER BY promoted_at NULLS FIRST, event_count DESC, blocks_seen DESC
		LIMIT $3`, int64(chainID), includePromoted, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidates(rows)
}

// PromotableCandidates returns unpromoted contracts that clear the activity
// thresholds, most active first.
func (s *Store) PromotableCandidates(ctx context.Context, chainID, minEvents, minBlocks uint64, limit int) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chain_id, address, standard, first_seen_block, last_seen_block,
		       event_count, blocks_seen, promoted_at, COALESCE(promotion_reason, '')
		FROM candidates
		WHERE chain_id = $1 AND promoted_at IS NULL
		  AND event_count >= $2 AND blocks_seen >= $3
		ORDER BY event_count DESC, blocks_seen DESC
		LIMIT $4`, int64(chainID), int64(minEvents), int64(minBlocks), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidates(rows)
}

// GetCandidate loads one observed contract.
func (s *Store) GetCandidate(ctx context.Context, chainID uint64, addr common.Address) (Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT chain_id, address, standard, first_seen_block, last_seen_block,
		       event_count, blocks_seen, promoted_at, COALESCE(promotion_reason, '')
		FROM candidates WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes())
	if err != nil {
		return Candidate{}, err
	}
	defer rows.Close()

	out, err := scanCandidates(rows)
	if err != nil {
		return Candidate{}, err
	}
	if len(out) == 0 {
		return Candidate{}, ErrNotFound
	}
	return out[0], nil
}

func scanCandidates(rows pgx.Rows) ([]Candidate, error) {
	var out []Candidate
	for rows.Next() {
		var (
			c                     Candidate
			cid                   int64
			addr                  []byte
			std                   int16
			first, last, ev, blks int64
		)
		if err := rows.Scan(&cid, &addr, &std, &first, &last, &ev, &blks,
			&c.PromotedAt, &c.PromotionReason); err != nil {
			return nil, err
		}
		c.ChainID = uint64(cid)
		c.Address = common.BytesToAddress(addr)
		c.Standard = uint8(std)
		c.FirstSeenBlock, c.LastSeenBlock = uint64(first), uint64(last)
		c.EventCount, c.BlocksSeen = uint64(ev), uint64(blks)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCandidatePromoted records that a candidate is now an indexed asset.
func (s *Store) MarkCandidatePromoted(ctx context.Context, chainID uint64, addr common.Address, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE candidates SET promoted_at = now(), promotion_reason = $3, updated_at = now()
		WHERE chain_id = $1 AND address = $2 AND promoted_at IS NULL`,
		int64(chainID), addr.Bytes(), reason)
	return err
}

// SetBackfillFloor records the block a backfill will stop at, so the API can report
// whether history is complete or merely as complete as the node allows.
func (s *Store) SetBackfillFloor(ctx context.Context, chainID uint64, addr common.Address, floor uint64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE asset_cursors SET backfill_floor = $3 WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes(), int64(floor))
	return err
}

// CandidateStats summarises discovery progress.
type CandidateStats struct {
	Observed   int64
	Promoted   int64
	Promotable int64
}

func (s *Store) CandidateStats(ctx context.Context, chainID, minEvents, minBlocks uint64) (CandidateStats, error) {
	var st CandidateStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1),
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1 AND promoted_at IS NOT NULL),
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1 AND promoted_at IS NULL
			   AND event_count >= $2 AND blocks_seen >= $3)`,
		int64(chainID), int64(minEvents), int64(minBlocks)).
		Scan(&st.Observed, &st.Promoted, &st.Promotable)
	return st, err
}
