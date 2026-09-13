package store

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Interaction is a rollup row: one account's relationship with one contract.
type Interaction struct {
	Account    common.Address
	Asset      common.Address
	FirstBlock uint64
	LastBlock  uint64
	EventCount uint64
	Roles      uint32 // bitmask over evmlog.Role
}

// PendingEvent is a single decoded participation awaiting confirmation.
type PendingEvent struct {
	BlockNumber uint64
	BlockHash   common.Hash
	LogIndex    uint32
	Asset       common.Address
	Account     common.Address
	Role        uint8
}

// FoldInteractions merges pre-aggregated rollup rows.
//
// Used by the backfill, which scans blocks far below the head and can therefore write
// straight into the rollup without passing through the pending table.
func (s *Store) FoldInteractions(ctx context.Context, chainID uint64, rows []Interaction) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
		INSERT INTO interactions
			(chain_id, account, asset, first_block, last_block, event_count, roles)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (chain_id, account, asset) DO UPDATE SET
			first_block = LEAST(interactions.first_block, EXCLUDED.first_block),
			last_block  = GREATEST(interactions.last_block, EXCLUDED.last_block),
			event_count = interactions.event_count + EXCLUDED.event_count,
			roles       = interactions.roles | EXCLUDED.roles,
			updated_at  = now()`

	return s.inTx(ctx, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, r := range rows {
			b.Queue(q, int64(chainID), r.Account.Bytes(), r.Asset.Bytes(),
				int64(r.FirstBlock), int64(r.LastBlock), int64(r.EventCount), int32(r.Roles))
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

// InsertPending buffers unconfirmed events.
//
// The primary key makes this idempotent, so re-scanning a range that is already
// buffered cannot double-count.
func (s *Store) InsertPending(ctx context.Context, chainID uint64, evs []PendingEvent) error {
	if len(evs) == 0 {
		return nil
	}
	const q = `
		INSERT INTO pending_events
			(chain_id, block_number, block_hash, log_index, asset, account, role)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (chain_id, block_number, log_index, account, role) DO NOTHING`

	return s.inTx(ctx, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, e := range evs {
			b.Queue(q, int64(chainID), int64(e.BlockNumber), e.BlockHash.Bytes(),
				int32(e.LogIndex), e.Asset.Bytes(), e.Account.Bytes(), int16(e.Role))
		}
		br := tx.SendBatch(ctx, b)
		defer br.Close()
		for range evs {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		return nil
	})
}

// PromotePending folds every buffered event at or below confirmedTo into the rollup
// and drops it from the buffer, atomically.
//
// This is the only path by which head-adjacent data reaches the rollup, which is what
// makes a reorg shallower than the confirmation lag harmless: the affected rows are
// still in pending_events and get deleted, never having polluted the aggregate.
func (s *Store) PromotePending(ctx context.Context, chainID, confirmedTo uint64) (int64, error) {
	var n int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH moved AS (
				DELETE FROM pending_events
				WHERE chain_id = $1 AND block_number <= $2
				RETURNING account, asset, block_number, log_index, role
			), agg AS (
				SELECT account,
				       asset,
				       MIN(block_number)                          AS first_block,
				       MAX(block_number)                          AS last_block,
				       COUNT(DISTINCT (block_number, log_index))  AS event_count,
				       bit_or((1::int << (role - 1)::int))        AS roles
				FROM moved
				GROUP BY account, asset
			), ins AS (
				INSERT INTO interactions
					(chain_id, account, asset, first_block, last_block, event_count, roles)
				SELECT $1, account, asset, first_block, last_block, event_count, roles FROM agg
				ON CONFLICT (chain_id, account, asset) DO UPDATE SET
					first_block = LEAST(interactions.first_block, EXCLUDED.first_block),
					last_block  = GREATEST(interactions.last_block, EXCLUDED.last_block),
					event_count = interactions.event_count + EXCLUDED.event_count,
					roles       = interactions.roles | EXCLUDED.roles,
					updated_at  = now()
				RETURNING 1
			)
			SELECT COUNT(*) FROM ins`,
			int64(chainID), int64(confirmedTo)).Scan(&n)
	})
	return n, err
}

// DropPendingFrom discards buffered events at or above fromBlock, for a reorg.
func (s *Store) DropPendingFrom(ctx context.Context, chainID, fromBlock uint64) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pending_events WHERE chain_id = $1 AND block_number >= $2`,
		int64(chainID), int64(fromBlock))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PendingBlockHashes returns the buffered block hashes at or above fromBlock, so the
// follower can compare them against the canonical chain.
func (s *Store) PendingBlockHashes(ctx context.Context, chainID, fromBlock uint64) (map[uint64]common.Hash, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT block_number, block_hash
		FROM pending_events WHERE chain_id = $1 AND block_number >= $2
		ORDER BY block_number`, int64(chainID), int64(fromBlock))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[uint64]common.Hash{}
	for rows.Next() {
		var n int64
		var h []byte
		if err := rows.Scan(&n, &h); err != nil {
			return nil, err
		}
		out[uint64(n)] = common.BytesToHash(h)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------------------
// Read paths
// --------------------------------------------------------------------------

// AccountAsset is one discovery hint, joined with what we know about the contract.
type AccountAsset struct {
	Asset      common.Address
	Standard   uint8
	Symbol     string
	Name       string
	Decimals   *int16
	FirstBlock uint64
	LastBlock  uint64
	EventCount uint64
	Roles      uint32
	Status     string
	// Reports is the operator's complaint count against the asset, carried so the
	// account view can sink a reported contract without a second query.
	Reports int
}

// AccountAssets answers the core question: which contracts has this account touched?
func (s *Store) AccountAssets(ctx context.Context, chainID uint64, account common.Address) ([]AccountAsset, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.asset, a.standard, COALESCE(a.symbol,''), COALESCE(a.name,''), a.decimals,
		       i.first_block, i.last_block, i.event_count, i.roles, a.status, a.reports
		FROM interactions i
		JOIN assets a ON a.chain_id = i.chain_id AND a.address = i.asset
		WHERE i.chain_id = $1 AND i.account = $2
		ORDER BY i.last_block DESC, i.asset`,
		int64(chainID), account.Bytes())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccountAsset
	for rows.Next() {
		var (
			r        AccountAsset
			asset    []byte
			standard int16
			fb, lb   int64
			ec       int64
			roles    int32
		)
		if err := rows.Scan(&asset, &standard, &r.Symbol, &r.Name, &r.Decimals,
			&fb, &lb, &ec, &roles, &r.Status, &r.Reports); err != nil {
			return nil, err
		}
		r.Asset = common.BytesToAddress(asset)
		r.Standard = uint8(standard)
		r.FirstBlock, r.LastBlock = uint64(fb), uint64(lb)
		r.EventCount = uint64(ec)
		r.Roles = uint32(roles)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AssetHolders lists accounts known to have touched a contract.
func (s *Store) AssetHolders(ctx context.Context, chainID uint64, asset common.Address, limit, offset int) ([]Interaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT account, asset, first_block, last_block, event_count, roles
		FROM interactions
		WHERE chain_id = $1 AND asset = $2
		ORDER BY last_block DESC, account
		LIMIT $3 OFFSET $4`,
		int64(chainID), asset.Bytes(), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Interaction
	for rows.Next() {
		var (
			r          Interaction
			acc, ast   []byte
			fb, lb, ec int64
			roles      int32
		)
		if err := rows.Scan(&acc, &ast, &fb, &lb, &ec, &roles); err != nil {
			return nil, err
		}
		r.Account = common.BytesToAddress(acc)
		r.Asset = common.BytesToAddress(ast)
		r.FirstBlock, r.LastBlock, r.EventCount = uint64(fb), uint64(lb), uint64(ec)
		r.Roles = uint32(roles)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AccountAssetSet is an account's asset list at a snapshot height, ascending by
// address so the merkle leaf encoding is deterministic.
type AccountAssetSet struct {
	Account common.Address
	Assets  []common.Address
}

// SnapshotIndex returns the whole index as of toBlock, for building a commitment.
//
// It materialises the result in memory, which is fine at the scale this design
// targets: the index is bounded by the registered asset set, not by chain size.
func (s *Store) SnapshotIndex(ctx context.Context, chainID, toBlock uint64) ([]AccountAssetSet, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT account, array_agg(asset ORDER BY asset)
		FROM interactions
		WHERE chain_id = $1 AND first_block <= $2
		GROUP BY account
		ORDER BY account`,
		int64(chainID), int64(toBlock))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccountAssetSet
	for rows.Next() {
		var acc []byte
		var assets [][]byte
		if err := rows.Scan(&acc, &assets); err != nil {
			return nil, err
		}
		set := AccountAssetSet{Account: common.BytesToAddress(acc)}
		for _, a := range assets {
			set.Assets = append(set.Assets, common.BytesToAddress(a))
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

// Stats summarises index size for the status endpoint.
type Stats struct {
	Assets       int64
	Accounts     int64
	Interactions int64
	Pending      int64
}

func (s *Store) Stats(ctx context.Context, chainID uint64) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM assets WHERE chain_id = $1 AND status <> 'revoked'),
			(SELECT COUNT(DISTINCT account) FROM interactions WHERE chain_id = $1),
			(SELECT COUNT(*) FROM interactions WHERE chain_id = $1),
			(SELECT COUNT(*) FROM pending_events WHERE chain_id = $1)`,
		int64(chainID)).Scan(&st.Assets, &st.Accounts, &st.Interactions, &st.Pending)
	return st, err
}

// CoverageRange reports the lowest and highest block the rollup currently covers for
// a chain, which becomes a commitment's declared range.
func (s *Store) CoverageRange(ctx context.Context, chainID uint64) (from, to uint64, err error) {
	var lo, hi *int64
	if err = s.pool.QueryRow(ctx,
		`SELECT MIN(first_block), MAX(last_block) FROM interactions WHERE chain_id = $1`,
		int64(chainID)).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	if lo == nil || hi == nil {
		return 0, 0, nil
	}
	return uint64(*lo), uint64(*hi), nil
}

// AccountRank is one account's whole footprint in the index.
type AccountRank struct {
	Account    common.Address
	AssetCount int64
	EventCount uint64
	FirstBlock uint64
	LastBlock  uint64
}

// RankAccounts lists the accounts in the index, busiest first. lo and hi bound the
// address range when the caller is filtering by prefix; nil means every account.
//
// This is a full aggregate over one chain's rollup, which sounds alarming and is not:
// interactions only ever holds rows for *promoted* assets, so the table is bounded by
// the curated asset set by construction. It is the same cost class as SnapshotIndex and
// Stats. Do not answer it from a maintained summary table — that would put a write on
// the fold path, where the cost would be paid on every block instead of on every visit
// to a page nobody keeps open.
func (s *Store) RankAccounts(ctx context.Context, chainID uint64, lo, hi []byte, limit, offset int) ([]AccountRank, error) {
	// SUM(event_count) is spelled out in the ORDER BY rather than aliased: an
	// unqualified event_count there would resolve to the input column, silently
	// ordering by one row's count instead of the account's total.
	rows, err := s.pool.Query(ctx, `
		SELECT account, COUNT(*), SUM(event_count), MIN(first_block), MAX(last_block)
		FROM interactions
		WHERE chain_id = $1
		  AND ($2::bytea IS NULL OR (account >= $2::bytea AND account <= $3::bytea))
		GROUP BY account
		ORDER BY SUM(event_count) DESC, account
		LIMIT $4 OFFSET $5`,
		int64(chainID), lo, hi, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccountRank
	for rows.Next() {
		var a AccountRank
		var acct []byte
		var assets, ev, first, last int64
		if err := rows.Scan(&acct, &assets, &ev, &first, &last); err != nil {
			return nil, err
		}
		a.Account = common.BytesToAddress(acct)
		a.AssetCount = assets
		a.EventCount = uint64(ev)
		a.FirstBlock, a.LastBlock = uint64(first), uint64(last)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AssetMembers is one contract and the accounts that touched it most, for the graph
// view. Holders is truncated to the requested width; HolderTotal says by how much.
type AssetMembers struct {
	Asset       common.Address
	Standard    uint8
	Symbol      string
	Name        string
	HolderTotal uint64
	Holders     []Interaction
}

// GraphMemberships returns a page of the busiest assets with their busiest accounts.
//
// This is the membership list behind the graph page, and it is deliberately a
// membership list rather than an edge list: an asset with k accounts implies
// k*(k-1)/2 pairs, so sending the pairs would send a square of what the caller
// needs to draw them.
//
// It is paged rather than capped so that a caller can keep going. The order —
// busiest asset first, by the logs_seen counter the follower keeps — is total and
// stable, so page n+1 is the next n assets and never a reshuffle of the ones
// already drawn. total is the number of assets that have any account at all, which
// is what says whether another page exists; it is carried on the rows, so a page
// past the end reports zero of both.
func (s *Store) GraphMemberships(ctx context.Context, chainID uint64, limit, offset, perAsset int) (rowsOut []AssetMembers, total uint64, err error) {
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
			-- Ordered by logs_seen, the counter the follower already maintains,
			-- rather than by a COUNT over interactions. Counting every asset's rows
			-- to decide which forty to show costs the whole table on every request,
			-- including every "load more"; this costs one index lookup per asset.
			-- EXISTS keeps an asset with no accounts out of the page and out of
			-- total, without counting its rows either.
			SELECT a.address, a.standard, a.symbol, a.name,
			       COALESCE(c.logs_seen, 0) AS activity,
			       COUNT(*) OVER () AS total
			FROM assets a
			LEFT JOIN asset_cursors c
			       ON c.chain_id = a.chain_id AND c.address = a.address
			WHERE a.chain_id = $1 AND a.status <> 'revoked'
			  AND EXISTS (SELECT 1 FROM interactions i
			               WHERE i.chain_id = a.chain_id AND i.asset = a.address)
			ORDER BY activity DESC, a.address
			LIMIT $2 OFFSET $3
		)
		SELECT r.address, r.standard, r.symbol, r.name, r.total,
		       -- Only the assets on this page are counted, so the cost is the page's,
		       -- not the index's.
		       (SELECT COUNT(*) FROM interactions i
		         WHERE i.chain_id = $1 AND i.asset = r.address) AS holders,
		       m.account, m.first_block, m.last_block, m.event_count, m.roles
		FROM ranked r
		JOIN LATERAL (
			SELECT account, first_block, last_block, event_count, roles
			FROM interactions i
			WHERE i.chain_id = $1 AND i.asset = r.address
			ORDER BY event_count DESC, account
			LIMIT $4
		) m ON TRUE
		ORDER BY r.activity DESC, r.address, m.event_count DESC, m.account`,
		int64(chainID), limit, offset, perAsset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []AssetMembers
	for rows.Next() {
		var (
			addr, acc    []byte
			standard     int16
			symbol, name *string
			holders, tot int64
			fb, lb, ec   int64
			roles        int32
		)
		if err := rows.Scan(&addr, &standard, &symbol, &name, &tot, &holders,
			&acc, &fb, &lb, &ec, &roles); err != nil {
			return nil, 0, err
		}
		total = uint64(tot)
		asset := common.BytesToAddress(addr)
		if len(out) == 0 || out[len(out)-1].Asset != asset {
			m := AssetMembers{Asset: asset, Standard: uint8(standard), HolderTotal: uint64(holders)}
			if symbol != nil {
				m.Symbol = *symbol
			}
			if name != nil {
				m.Name = *name
			}
			out = append(out, m)
		}
		cur := &out[len(out)-1]
		cur.Holders = append(cur.Holders, Interaction{
			Account:    common.BytesToAddress(acc),
			Asset:      asset,
			FirstBlock: uint64(fb),
			LastBlock:  uint64(lb),
			EventCount: uint64(ec),
			Roles:      uint32(roles),
		})
	}
	return out, total, rows.Err()
}
