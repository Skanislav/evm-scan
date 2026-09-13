package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// MaxAssetCommitItems keeps one signed list bounded: it is a recent wallet sweep,
// not an unbounded token registry.
const MaxAssetCommitItems = 200

// AssetCommitItem is one exact chain and contract pair a reader signed for.
type AssetCommitItem struct {
	ChainID uint64
	Asset   common.Address
}

// AssetCommit is the latest exact list an account signed. It replaces rather than
// accumulates, because a list is a snapshot of a sweep, not an assertion forever.
type AssetCommit struct {
	Account  common.Address
	Items    []AssetCommitItem
	Digest   common.Hash
	Deadline int64
	SignedAt time.Time
}

// ErrStaleAssetCommit means a stored commitment has an equal or later deadline.
var ErrStaleAssetCommit = errors.New("store: a newer asset commit is already stored")

// AssetCommit returns an account's latest exact signed list, or ErrNotFound.
func (s *Store) AssetCommit(ctx context.Context, account common.Address) (AssetCommit, error) {
	var (
		c      AssetCommit
		acct   []byte
		digest []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT account, digest, deadline, signed_at
		FROM account_asset_commits
		WHERE account = $1`, account.Bytes()).Scan(&acct, &digest, &c.Deadline, &c.SignedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetCommit{}, ErrNotFound
	}
	if err != nil {
		return AssetCommit{}, err
	}
	c.Account = common.BytesToAddress(acct)
	c.Digest = common.BytesToHash(digest)

	rows, err := s.pool.Query(ctx, `
		SELECT chain_id, asset
		FROM account_asset_commit_items
		WHERE account = $1
		ORDER BY chain_id, asset`, account.Bytes())
	if err != nil {
		return AssetCommit{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			chainID int64
			asset   []byte
		)
		if err := rows.Scan(&chainID, &asset); err != nil {
			return AssetCommit{}, err
		}
		if chainID < 0 {
			return AssetCommit{}, fmt.Errorf("store: negative asset commit chain id")
		}
		c.Items = append(c.Items, AssetCommitItem{ChainID: uint64(chainID), Asset: common.BytesToAddress(asset)})
	}
	if err := rows.Err(); err != nil {
		return AssetCommit{}, err
	}
	return c, nil
}

// PutAssetCommit replaces an account's list only when its deadline rises. The
// commitment row moves first inside the transaction, so readers see either the old
// complete list or the new complete list, never a partially rewritten one.
func (s *Store) PutAssetCommit(ctx context.Context, c AssetCommit) error {
	if len(c.Items) > MaxAssetCommitItems {
		return fmt.Errorf("store: an asset commit names at most %d pairs", MaxAssetCommitItems)
	}
	if c.Deadline < 0 {
		return errors.New("store: asset commit deadline must be non-negative")
	}
	items := append([]AssetCommitItem(nil), c.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].ChainID != items[j].ChainID {
			return items[i].ChainID < items[j].ChainID
		}
		return bytes.Compare(items[i].Asset[:], items[j].Asset[:]) < 0
	})
	for i, item := range items {
		if item.ChainID == 0 || item.ChainID > uint64(1<<63-1) {
			return errors.New("store: asset commit chain id must be a positive int64")
		}
		if i > 0 && items[i-1].ChainID == item.ChainID && items[i-1].Asset == item.Asset {
			return fmt.Errorf("store: asset commit names %s twice", item.Asset.Hex())
		}
	}

	return s.inTx(ctx, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			INSERT INTO account_asset_commits (account, digest, deadline, signed_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (account) DO UPDATE
			SET digest = EXCLUDED.digest, deadline = EXCLUDED.deadline, signed_at = now()
			WHERE account_asset_commits.deadline < EXCLUDED.deadline`,
			c.Account.Bytes(), c.Digest.Bytes(), c.Deadline)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrStaleAssetCommit
		}
		if _, err := tx.Exec(ctx, `DELETE FROM account_asset_commit_items WHERE account = $1`, c.Account.Bytes()); err != nil {
			return err
		}
		for _, item := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO account_asset_commit_items (account, chain_id, asset)
				VALUES ($1, $2, $3)`, c.Account.Bytes(), int64(item.ChainID), item.Asset.Bytes()); err != nil {
				return err
			}
		}
		return nil
	})
}

// AssetCommitDeadline is kept as a big integer at the API boundary because the
// signature covers a uint256. PostgreSQL stores only unix seconds, which fit int64.
func AssetCommitDeadline(deadline *big.Int) (int64, error) {
	if deadline == nil || !deadline.IsInt64() || deadline.Sign() < 0 {
		return 0, errors.New("store: asset commit deadline must be an int64 unix time")
	}
	return deadline.Int64(), nil
}
