package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Trust levels. See migrations/0006_chain_profiles.sql for what each one may do;
// the short version is that only a verified chain's data may back a bonded
// commitment, because a bond is money staked on logs we did not verify.
const (
	TrustVerified    = "verified"
	TrustUnverified  = "unverified"
	TrustQuarantined = "quarantined"
)

// Where a chain came from.
const (
	ChainSourceConfig = "config" // named in the YAML, authoritative there
	ChainSourceAPI    = "api"    // added over /v1/chains
	ChainSourceDemand = "demand" // added because the registry showed paid demand
)

// ChainProfile is everything needed to run a chain, as persisted.
//
// It is the database's view, not the config's: NodeURL here is the stored secret,
// and Tuning is the JSON blob holding what a config.Chain would tune.
type ChainProfile struct {
	ChainID uint64
	Name    string
	Enabled bool
	Source  string
	Trust   string
	// NodeURL is a secret — it usually carries a provider's API key. Nothing may
	// put it in an API response; use chain.Endpoint.Redacted for display.
	NodeURL string

	NativeSymbol   string
	NativeDecimals int16
	WrappedNative  *common.Address

	// Tuning is the per-chain knobs, stored as one JSON blob because it is written
	// and read whole and never queried into. ChainTuning is its shape.
	Tuning json.RawMessage

	ENSName     string
	ENSResolved *time.Time

	TrustSetAt *time.Time
	TrustSetBy string

	LastError   string
	LastErrorAt *time.Time
}

// UpsertChainProfile writes a chain's whole record.
//
// A config chain outranks a stored one: config is authoritative for the chains it
// names, so re-running with an edited YAML must win. What it does *not* overwrite
// is a trust promotion, which an operator made deliberately and which the config
// file has no opinion about.
func (s *Store) UpsertChainProfile(ctx context.Context, p ChainProfile) error {
	if p.ChainID == 0 {
		return errors.New("store: chain profile needs a chain id")
	}
	if p.Trust == "" {
		p.Trust = TrustUnverified
	}
	if p.Source == "" {
		p.Source = ChainSourceConfig
	}
	if p.NativeDecimals == 0 {
		p.NativeDecimals = 18
	}
	if len(p.Tuning) == 0 {
		p.Tuning = json.RawMessage(`{}`)
	}
	var wrapped []byte
	if p.WrappedNative != nil {
		wrapped = p.WrappedNative.Bytes()
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO chains (chain_id, name, enabled, source, trust, node_url,
		                    native_symbol, native_decimals, wrapped_native, profile,
		                    ens_name, ens_resolved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12)
		ON CONFLICT (chain_id) DO UPDATE SET
			name            = EXCLUDED.name,
			enabled         = EXCLUDED.enabled,
			source          = EXCLUDED.source,
			node_url        = COALESCE(EXCLUDED.node_url, chains.node_url),
			native_symbol   = COALESCE(EXCLUDED.native_symbol, chains.native_symbol),
			native_decimals = EXCLUDED.native_decimals,
			wrapped_native  = COALESCE(EXCLUDED.wrapped_native, chains.wrapped_native),
			profile         = EXCLUDED.profile,
			ens_name        = COALESCE(EXCLUDED.ens_name, chains.ens_name),
			ens_resolved_at = COALESCE(EXCLUDED.ens_resolved_at, chains.ens_resolved_at),
			-- Trust is not config's to set. An operator raised it on purpose and a
			-- restart must not quietly undo that, nor quietly redo it.
			trust           = chains.trust,
			updated_at      = now()`,
		int64(p.ChainID), p.Name, p.Enabled, p.Source, p.Trust, nullStr(p.NodeURL),
		nullStr(p.NativeSymbol), p.NativeDecimals, wrapped, []byte(p.Tuning),
		p.ENSName, p.ENSResolved)
	return err
}

// SetChainTrust records a deliberate change of trust level.
//
// `by` is free text naming who decided — an operator label, not an identity the
// system can check. It is written so the decision can be explained later, which is
// the whole reason this is not just an UPDATE of one column.
func (s *Store) SetChainTrust(ctx context.Context, chainID uint64, trust, by string) error {
	switch trust {
	case TrustVerified, TrustUnverified, TrustQuarantined:
	default:
		return fmt.Errorf("store: %q is not a trust level", trust)
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE chains
		SET trust = $2, trust_set_at = now(), trust_set_by = NULLIF($3,''), updated_at = now()
		WHERE chain_id = $1`,
		int64(chainID), trust, by)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetChainEnabled turns a chain on or off without losing what it has indexed.
func (s *Store) SetChainEnabled(ctx context.Context, chainID uint64, enabled bool) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE chains SET enabled = $2, updated_at = now() WHERE chain_id = $1`,
		int64(chainID), enabled)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetChainError records why a chain is not running, or clears it when err is nil.
func (s *Store) SetChainError(ctx context.Context, chainID uint64, cause error) error {
	if cause == nil {
		_, err := s.pool.Exec(ctx, `
			UPDATE chains SET last_error = NULL, last_error_at = NULL WHERE chain_id = $1`,
			int64(chainID))
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE chains SET last_error = $2, last_error_at = now() WHERE chain_id = $1`,
		int64(chainID), cause.Error())
	return err
}

const chainProfileCols = `chain_id, name, enabled, source, trust, node_url,
	COALESCE(native_symbol,''), native_decimals, wrapped_native, profile,
	COALESCE(ens_name,''), ens_resolved_at, trust_set_at, COALESCE(trust_set_by,''),
	COALESCE(last_error,''), last_error_at`

// GetChainProfile loads one chain.
func (s *Store) GetChainProfile(ctx context.Context, chainID uint64) (ChainProfile, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+chainProfileCols+` FROM chains WHERE chain_id = $1`, int64(chainID))
	p, err := scanChainProfile(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChainProfile{}, ErrNotFound
	}
	return p, err
}

// ListChainProfiles returns every recorded chain, enabled first then by id, so the
// order is stable for display.
func (s *Store) ListChainProfiles(ctx context.Context, enabledOnly bool) ([]ChainProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+chainProfileCols+` FROM chains
		 WHERE ($1 = FALSE OR enabled)
		 ORDER BY enabled DESC, chain_id`, enabledOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChainProfile
	for rows.Next() {
		p, err := scanChainProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanChainProfile(r scannable) (ChainProfile, error) {
	var (
		p       ChainProfile
		chainID int64
		nodeURL *string
		wrapped []byte
		tuning  []byte
	)
	if err := r.Scan(&chainID, &p.Name, &p.Enabled, &p.Source, &p.Trust, &nodeURL,
		&p.NativeSymbol, &p.NativeDecimals, &wrapped, &tuning,
		&p.ENSName, &p.ENSResolved, &p.TrustSetAt, &p.TrustSetBy,
		&p.LastError, &p.LastErrorAt); err != nil {
		return ChainProfile{}, err
	}
	p.ChainID = uint64(chainID)
	if nodeURL != nil {
		p.NodeURL = *nodeURL
	}
	if len(wrapped) == 20 {
		a := common.BytesToAddress(wrapped)
		p.WrappedNative = &a
	}
	p.Tuning = json.RawMessage(tuning)
	return p, nil
}

// ChainTuning is the shape of ChainProfile.Tuning.
//
// It lives here, next to the column that holds it, because both ends of the round
// trip need it: the API writes one when a chain is added, and the daemon reads it
// back to build an indexer. Two independent copies of these field names would be a
// silent-drift bug waiting for somebody to rename a JSON tag on one side only.
//
// Durations are strings so the blob stays readable in psql — "2s" rather than
// 2000000000 — and so an unset one is distinguishable from a zero.
type ChainTuning struct {
	Confirmations    uint64 `json:"confirmations"`
	BackfillWindow   uint64 `json:"backfill_window"`
	TailWindow       uint64 `json:"tail_window"`
	PollInterval     string `json:"poll_interval"`
	BackfillInterval string `json:"backfill_interval"`
	Discovery        struct {
		Enabled  bool   `json:"enabled"`
		Lookback uint64 `json:"lookback"`
		Interval string `json:"interval"`
	} `json:"discovery"`
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
