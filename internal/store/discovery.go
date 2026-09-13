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
	// SpamAt is set when an operator has ruled the contract not worth indexing. A
	// candidate carries at most one live verdict: promoting clears this.
	SpamAt     *time.Time
	SpamReason string
	// Voters is how many distinct accounts asked for this contract, through the API
	// and on the registry together. A priority signal, never a verdict.
	Voters uint64
	// Against is how many signed against it; promotion reads Voters minus Against.
	Against uint64
}

// PromotionRule is what qualifies a candidate for automatic promotion. Either leg
// alone is enough: activity when Activity is on, or demand when MinVoters is above
// zero. Demand goes first in the order, because a contract people asked for beats
// one that is merely busy.
type PromotionRule struct {
	Activity  bool
	MinEvents uint64
	MinBlocks uint64
	MinVoters uint64
}

// CandidateFilter narrows a candidate listing. The zero value is the working view —
// contracts nobody has judged yet, which is exactly what the promotion queue is.
type CandidateFilter struct {
	IncludePromoted bool
	IncludeSpam     bool
	Limit           int
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
func (s *Store) ListCandidates(ctx context.Context, chainID uint64, f CandidateFilter) ([]Candidate, error) {
	// Spam sorts last rather than being interleaved: (spam_at IS NOT NULL) is false for
	// every unjudged row, so with nothing marked the order is byte-identical to what it
	// was before verdicts existed.
	rows, err := s.pool.Query(ctx, `
		SELECT c.chain_id, c.address, c.standard, c.first_seen_block, c.last_seen_block,
		       c.event_count, c.blocks_seen, c.promoted_at, COALESCE(c.promotion_reason, ''),
		       c.spam_at, COALESCE(c.spam_reason, ''), COALESCE(t.voters, 0), COALESCE(t.against, 0)
		FROM candidates c
		LEFT JOIN asset_demand_totals t ON t.chain_id = c.chain_id AND t.address = c.address
		WHERE c.chain_id = $1
		  AND ($2 OR c.promoted_at IS NULL)
		  AND ($3 OR c.spam_at IS NULL)
		ORDER BY (c.spam_at IS NOT NULL), c.promoted_at NULLS FIRST, COALESCE(t.voters, 0) DESC,
		         c.event_count DESC, c.blocks_seen DESC
		LIMIT $4`, int64(chainID), f.IncludePromoted, f.IncludeSpam, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidates(rows)
}

// PromotableCandidates returns unpromoted, unjudged contracts that clear the rule,
// most wanted first and then most active. spam_at is the whole spam rule: a
// verdict drops a contract here and nowhere else. Demand is net — signers for
// minus signers against — so a contract more readers marked as junk than
// recognized never clears min_voters, however many recognized it.
func (s *Store) PromotableCandidates(ctx context.Context, chainID uint64, rule PromotionRule, limit int) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.chain_id, c.address, c.standard, c.first_seen_block, c.last_seen_block,
		       c.event_count, c.blocks_seen, c.promoted_at, COALESCE(c.promotion_reason, ''),
		       c.spam_at, COALESCE(c.spam_reason, ''), COALESCE(t.voters, 0), COALESCE(t.against, 0)
		FROM candidates c
		LEFT JOIN asset_demand_totals t ON t.chain_id = c.chain_id AND t.address = c.address
		WHERE c.chain_id = $1 AND c.promoted_at IS NULL AND c.spam_at IS NULL
		  AND (($2 AND c.event_count >= $3 AND c.blocks_seen >= $4)
		       OR ($5 > 0 AND (COALESCE(t.voters, 0) - COALESCE(t.against, 0)) >= $5))
		ORDER BY (COALESCE(t.voters, 0) - COALESCE(t.against, 0)) DESC, c.event_count DESC, c.blocks_seen DESC
		LIMIT $6`, int64(chainID), rule.Activity, int64(rule.MinEvents), int64(rule.MinBlocks), int64(rule.MinVoters), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCandidates(rows)
}

// GetCandidate loads one observed contract.
func (s *Store) GetCandidate(ctx context.Context, chainID uint64, addr common.Address) (Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.chain_id, c.address, c.standard, c.first_seen_block, c.last_seen_block,
		       c.event_count, c.blocks_seen, c.promoted_at, COALESCE(c.promotion_reason, ''),
		       c.spam_at, COALESCE(c.spam_reason, ''), COALESCE(t.voters, 0), COALESCE(t.against, 0)
		FROM candidates c
		LEFT JOIN asset_demand_totals t ON t.chain_id = c.chain_id AND t.address = c.address
		WHERE c.chain_id = $1 AND c.address = $2`,
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
			voters, against       int64
		)
		if err := rows.Scan(&cid, &addr, &std, &first, &last, &ev, &blks,
			&c.PromotedAt, &c.PromotionReason, &c.SpamAt, &c.SpamReason, &voters, &against); err != nil {
			return nil, err
		}
		c.ChainID = uint64(cid)
		c.Address = common.BytesToAddress(addr)
		c.Standard = uint8(std)
		c.FirstSeenBlock, c.LastSeenBlock = uint64(first), uint64(last)
		c.EventCount, c.BlocksSeen = uint64(ev), uint64(blks)
		c.Voters = uint64(voters)
		c.Against = uint64(against)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkCandidatePromoted records that a candidate is now an indexed asset.
//
// Promoting clears any spam mark. Promotion is the strictly stronger and more expensive
// verdict, and auto-promote cannot reach a spam row at all, so the only way here is an
// operator deliberately overriding themselves. Leaving both timestamps set would make a
// row simultaneously spam and indexed, which is not a state the ledger can render.
func (s *Store) MarkCandidatePromoted(ctx context.Context, chainID uint64, addr common.Address, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE candidates
		SET promoted_at = now(), promotion_reason = $3,
		    spam_at = NULL, spam_reason = NULL, updated_at = now()
		WHERE chain_id = $1 AND address = $2 AND promoted_at IS NULL`,
		int64(chainID), addr.Bytes(), reason)
	return err
}

// MarkCandidateSpam records a verdict that a contract is not worth indexing.
//
// Only an unpromoted candidate can be marked: once a contract is an indexed asset the
// way back is revoking the asset, not editing the note discovery left behind.
func (s *Store) MarkCandidateSpam(ctx context.Context, chainID uint64, addr common.Address, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE candidates SET spam_at = now(), spam_reason = $3, updated_at = now()
		WHERE chain_id = $1 AND address = $2 AND promoted_at IS NULL`,
		int64(chainID), addr.Bytes(), reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Two ways to affect nothing, and they are different answers to the caller.
		if _, err := s.GetCandidate(ctx, chainID, addr); err != nil {
			return err
		}
		return ErrAlreadyPromoted
	}
	return nil
}

// ClearCandidateSpam puts a candidate back in the ranking. Idempotent: clearing a mark
// that is not there is not an error, only clearing one on a contract nobody has seen.
func (s *Store) ClearCandidateSpam(ctx context.Context, chainID uint64, addr common.Address) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE candidates SET spam_at = NULL, spam_reason = NULL, updated_at = now()
		WHERE chain_id = $1 AND address = $2`,
		int64(chainID), addr.Bytes())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	Spam       int64
}

func (s *Store) CandidateStats(ctx context.Context, chainID, minEvents, minBlocks uint64) (CandidateStats, error) {
	var st CandidateStats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1),
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1 AND promoted_at IS NOT NULL),
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1 AND promoted_at IS NULL
			   AND spam_at IS NULL AND event_count >= $2 AND blocks_seen >= $3),
			(SELECT COUNT(*) FROM candidates WHERE chain_id = $1 AND spam_at IS NOT NULL)`,
		int64(chainID), int64(minEvents), int64(minBlocks)).
		Scan(&st.Observed, &st.Promoted, &st.Promotable, &st.Spam)
	return st, err
}

// OldestCandidateBlock returns the earliest block in which discovery saw a watched
// event on this chain, and whether there is one. That row exists only because
// eth_getLogs returned at least one log for the block, which makes it a block the
// node has demonstrably served — the anchor chain.ProbeHistoryFloor uses to catch a
// node that answers pruned blocks with an empty result.
func (s *Store) OldestCandidateBlock(ctx context.Context, chainID uint64) (block uint64, ok bool, err error) {
	var b *int64
	err = s.pool.QueryRow(ctx,
		`SELECT MIN(first_seen_block) FROM candidates WHERE chain_id = $1`,
		int64(chainID)).Scan(&b)
	if err != nil {
		return 0, false, err
	}
	if b == nil {
		return 0, false, nil
	}
	return uint64(*b), true, nil
}

// Decision is a recorded verdict on a discovered contract. There is one per candidate,
// not one per event: promoting supersedes a spam mark rather than sitting beside it, so
// the ledger reports the call that stands rather than the history of calls made.
type Decision struct {
	ChainID  uint64
	Address  common.Address
	Standard uint8
	Verdict  string // "approved" | "spam"
	At       time.Time
	Reason   string
	Symbol   string
	Name     string
}

// RecentDecisions lists verdicts newest first.
//
// One ordered scan rather than a union of a promoted branch and a spam branch: at most
// one mark is live per row, so the union would emit the same rows through two scans.
// The ORDER BY is exactly candidates_verdict_idx.
//
// The join is the metadata shortcut. A promoted candidate was probed on its way into
// assets, so its symbol and name are already stored and the common half of the ledger
// costs no node call at all; only spam rows, which are never probed, come back blank
// for the caller to fill in.
func (s *Store) RecentDecisions(ctx context.Context, chainID uint64, limit int) ([]Decision, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.address, c.standard,
		       CASE WHEN c.promoted_at IS NOT NULL THEN 'approved' ELSE 'spam' END,
		       COALESCE(c.promoted_at, c.spam_at),
		       COALESCE(NULLIF(c.promotion_reason, ''), NULLIF(c.spam_reason, ''), ''),
		       COALESCE(a.symbol, ''), COALESCE(a.name, '')
		FROM candidates c
		LEFT JOIN assets a ON a.chain_id = c.chain_id AND a.address = c.address
		WHERE c.chain_id = $1 AND (c.promoted_at IS NOT NULL OR c.spam_at IS NOT NULL)
		ORDER BY COALESCE(c.promoted_at, c.spam_at) DESC
		LIMIT $2`, int64(chainID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Decision
	for rows.Next() {
		d := Decision{ChainID: chainID}
		var addr []byte
		var std int16
		if err := rows.Scan(&addr, &std, &d.Verdict, &d.At, &d.Reason, &d.Symbol, &d.Name); err != nil {
			return nil, err
		}
		d.Address = common.BytesToAddress(addr)
		d.Standard = uint8(std)
		out = append(out, d)
	}
	return out, rows.Err()
}
