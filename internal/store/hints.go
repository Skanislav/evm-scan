package store

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// AccountHint is a reader's own cross-chain bloom, kept because the account signed
// for it. See migrations/0011_account_hints.sql for what that row is and is not.
type AccountHint struct {
	Account  common.Address
	Bytes    []byte
	Digest   common.Hash
	Deadline int64
	SignedAt time.Time
}

// ErrStaleHint: the stored hint carries a later deadline than the one offered, so
// the offered one is an older signature and does not replace it.
var ErrStaleHint = errors.New("store: a newer hint is already stored")

// AccountHint returns the stored hint, or ErrNotFound.
func (s *Store) AccountHint(ctx context.Context, account common.Address) (AccountHint, error) {
	var (
		h      AccountHint
		acct   []byte
		digest []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT account, bytes, digest, deadline, signed_at FROM account_hints WHERE account = $1`,
		account.Bytes()).Scan(&acct, &h.Bytes, &digest, &h.Deadline, &h.SignedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountHint{}, ErrNotFound
	}
	if err != nil {
		return AccountHint{}, err
	}
	h.Account = common.BytesToAddress(acct)
	h.Digest = common.BytesToHash(digest)
	return h, nil
}

// PutAccountHint stores a hint, replacing an older one only when the deadline rises.
func (s *Store) PutAccountHint(ctx context.Context, h AccountHint) error {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO account_hints (account, bytes, digest, deadline, signed_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (account) DO UPDATE
		SET bytes = EXCLUDED.bytes, digest = EXCLUDED.digest, deadline = EXCLUDED.deadline, signed_at = now()
		WHERE account_hints.deadline < EXCLUDED.deadline`,
		h.Account.Bytes(), h.Bytes, h.Digest.Bytes(), h.Deadline)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrStaleHint
	}
	return nil
}
