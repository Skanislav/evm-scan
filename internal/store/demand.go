package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
)

// Demand is who wants a contract indexed.
//
// A reader who looks a wallet up can vote for the contracts it holds that the index
// does not keep. Votes decide what gets promoted first and nothing else: a spam
// verdict still drops a contract from the promotable set, and no vote ever changes
// what a balance read says. Two sources add up to one total — votes through this
// deployment's API, one per (asset, voter), and the registry's own on-chain counter
// mirrored as an aggregate.
//
// The voter is stored as keccak256(salt || chainId || account) under a salt generated
// once at migration time. That is enough to count an account once, and not enough to
// list who holds what: the table cannot be walked back to accounts, only tested
// against a guess by someone who also holds the salt — the same shape as a blinded
// filter, and the reason a vote does not become the per-account row the daemon has
// never kept for unindexed contracts.

// DemandRow is one contract's demand, with where it stands in the index.
type DemandRow struct {
	ChainID uint64
	Address common.Address
	// Voters is the total for: distinct API voters who signed +1 (or voted through
	// /v1/demand) plus the on-chain count. Against is the API voters who signed -1;
	// the registry counts nothing against.
	Voters        uint64
	Against       uint64
	OnchainVoters uint64
	LastAt        time.Time
	// Indexed is true once an asset row exists; Candidate when discovery has seen
	// it; Spam when a curator has ruled it out.
	Indexed    bool
	Candidate  bool
	Spam       bool
	EventCount uint64
}

// MaxDemandPerVote caps one request, so a vote is a wallet and not a token list.
const MaxDemandPerVote = 200

var demandSaltCache struct {
	mu   sync.Mutex
	salt map[*Store][]byte
}

func (s *Store) demandSalt(ctx context.Context) ([]byte, error) {
	demandSaltCache.mu.Lock()
	defer demandSaltCache.mu.Unlock()
	if demandSaltCache.salt == nil {
		demandSaltCache.salt = map[*Store][]byte{}
	}
	if b, ok := demandSaltCache.salt[s]; ok {
		return b, nil
	}
	var v string
	if err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'demand_salt'`).Scan(&v); err != nil {
		return nil, fmt.Errorf("store: demand salt: %w", err)
	}
	b := []byte(v)
	demandSaltCache.salt[s] = b
	return b, nil
}

// voterKey blinds an account under the deployment's salt.
func voterKey(salt []byte, chainID uint64, account common.Address) []byte {
	buf := make([]byte, 0, len(salt)+8+20)
	buf = append(buf, salt...)
	buf = binary.BigEndian.AppendUint64(buf, chainID)
	buf = append(buf, account.Bytes()...)
	return crypto.Keccak256(buf)
}

// RecordDemand counts one account's vote for each of addrs. A repeat vote from the
// same account for the same contract bumps its timestamp and counts once. Returns
// how many (asset, voter) pairs were new.
func (s *Store) RecordDemand(ctx context.Context, chainID uint64, account common.Address, addrs []common.Address) (int, error) {
	if len(addrs) == 0 {
		return 0, nil
	}
	if len(addrs) > MaxDemandPerVote {
		return 0, fmt.Errorf("store: a vote names at most %d contracts", MaxDemandPerVote)
	}
	salt, err := s.demandSalt(ctx)
	if err != nil {
		return 0, err
	}
	voter := voterKey(salt, chainID, account)

	added := 0
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		seen := map[common.Address]bool{}
		for _, a := range addrs {
			if seen[a] {
				continue
			}
			seen[a] = true
			var isNew bool
			if err := tx.QueryRow(ctx, `
				INSERT INTO asset_demand (chain_id, address, voter)
				VALUES ($1, $2, $3)
				ON CONFLICT (chain_id, address, voter) DO UPDATE
				SET votes = asset_demand.votes + 1, last_at = now()
				RETURNING (xmax = 0)`,
				int64(chainID), a.Bytes(), voter).Scan(&isNew); err != nil {
				return err
			}
			if isNew {
				added++
			}
		}
		return nil
	})
	return added, err
}

// SetOnchainDemand stores the registry's counter for one asset, as mirrored.
func (s *Store) SetOnchainDemand(ctx context.Context, chainID uint64, addr common.Address, voters uint64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO asset_demand_onchain (chain_id, address, voters, synced_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (chain_id, address) DO UPDATE SET voters = EXCLUDED.voters, synced_at = now()`,
		int64(chainID), addr.Bytes(), int64(voters))
	return err
}

// ReplaceOnchainDemand makes the mirrored aggregate exactly what the registry lists:
// rows present are written, rows absent are deleted. There is one registry per
// deployment and it is the truth for every chain, so a count synced from a registry
// this deployment has since moved away from does not linger as a phantom voter.
func (s *Store) ReplaceOnchainDemand(ctx context.Context, rows []DemandRow) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM asset_demand_onchain`); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := tx.Exec(ctx, `
				INSERT INTO asset_demand_onchain (chain_id, address, voters, synced_at)
				VALUES ($1, $2, $3, now())
				ON CONFLICT (chain_id, address) DO UPDATE SET voters = EXCLUDED.voters, synced_at = now()`,
				int64(r.ChainID), r.Address.Bytes(), int64(r.OnchainVoters)); err != nil {
				return err
			}
		}
		return nil
	})
}

const demandSelect = `
	SELECT t.chain_id, t.address, t.voters, t.against, COALESCE(o.voters, 0),
	       COALESCE(d.last_at, o.synced_at, now()),
	       (a.address IS NOT NULL), (c.address IS NOT NULL), (c.spam_at IS NOT NULL),
	       COALESCE(c.event_count, 0)
	FROM asset_demand_totals t
	LEFT JOIN asset_demand_onchain o USING (chain_id, address)
	LEFT JOIN (SELECT chain_id, address, MAX(last_at) AS last_at FROM asset_demand GROUP BY 1, 2) d USING (chain_id, address)
	LEFT JOIN assets a ON a.chain_id = t.chain_id AND a.address = t.address
	LEFT JOIN candidates c ON c.chain_id = t.chain_id AND c.address = t.address`

// ListDemand returns the most wanted contracts on a chain, largest net (for minus
// against) first.
func (s *Store) ListDemand(ctx context.Context, chainID uint64, limit int) ([]DemandRow, error) {
	rows, err := s.pool.Query(ctx, demandSelect+`
		WHERE t.chain_id = $1
		ORDER BY (t.voters - t.against) DESC, t.voters DESC, d.last_at DESC NULLS LAST
		LIMIT $2`, int64(chainID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemand(rows)
}

// DemandedUnseen returns voted contracts that discovery has never counted and the
// index does not keep, whose net demand (for minus against) is at or above
// minVoters. Discovery only counts what emitted an event inside its window, so a
// contract a wallet holds quietly can be wanted and still have no candidate row;
// this is how it reaches promotion.
func (s *Store) DemandedUnseen(ctx context.Context, chainID, minVoters uint64, limit int) ([]DemandRow, error) {
	rows, err := s.pool.Query(ctx, demandSelect+`
		WHERE t.chain_id = $1 AND (t.voters - t.against) >= $2 AND a.address IS NULL AND c.address IS NULL
		ORDER BY (t.voters - t.against) DESC
		LIMIT $3`, int64(chainID), int64(minVoters), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemand(rows)
}

// DemandTotals is one contract's count each way.
type DemandTotals struct {
	For     uint64
	Against uint64
}

// Net is for minus against, which is what promotion and ordering read.
func (d DemandTotals) Net() int64 { return int64(d.For) - int64(d.Against) }

// DemandFor reports the totals for each of addrs; absent means zero both ways.
func (s *Store) DemandFor(ctx context.Context, chainID uint64, addrs []common.Address) (map[common.Address]DemandTotals, error) {
	out := map[common.Address]DemandTotals{}
	if len(addrs) == 0 {
		return out, nil
	}
	raw := make([][]byte, len(addrs))
	for i, a := range addrs {
		raw[i] = a.Bytes()
	}
	rows, err := s.pool.Query(ctx, `
		SELECT address, voters, against FROM asset_demand_totals
		WHERE chain_id = $1 AND address = ANY($2)`, int64(chainID), raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var addr []byte
		var n, ag int64
		if err := rows.Scan(&addr, &n, &ag); err != nil {
			return nil, err
		}
		out[common.BytesToAddress(addr)] = DemandTotals{For: uint64(n), Against: uint64(ag)}
	}
	return out, rows.Err()
}

func scanDemand(rows pgx.Rows) ([]DemandRow, error) {
	var out []DemandRow
	for rows.Next() {
		var (
			r                         DemandRow
			cid, voters, against, onc int64
			addr                      []byte
			ev                        int64
		)
		if err := rows.Scan(&cid, &addr, &voters, &against, &onc, &r.LastAt, &r.Indexed, &r.Candidate, &r.Spam, &ev); err != nil {
			return nil, err
		}
		r.ChainID = uint64(cid)
		r.Address = common.BytesToAddress(addr)
		r.Voters = uint64(voters)
		r.Against = uint64(against)
		r.OnchainVoters = uint64(onc)
		r.EventCount = uint64(ev)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --------------------------------------------------------------------------
// Verdicts: one signed split, replacing whatever the voter said before.
// --------------------------------------------------------------------------

// Verdict is one contract's direction in a reader's signed split: +1 recognized,
// -1 not. A contract the reader left out has no row, which is what 0 means.
type Verdict struct {
	Address common.Address
	Weight  int8
}

// ErrStaleVerdict: the stored deadline for this (chain, voter) is at or past the
// one offered, so the offered signature is an older one and does not replace it.
var ErrStaleVerdict = errors.New("store: a verdict with a later deadline is already stored")

// ReplaceVerdicts makes the voter's rows on a chain exactly the pairs given: rows
// for contracts not named are deleted, the rest are upserted with their weight, and
// the deadline is stored as the new floor for the next signature. All in one
// transaction, so a reader never sees half a split. Returns how many rows were
// written and how many were cleared. An empty list is a valid "retract everything".
func (s *Store) ReplaceVerdicts(ctx context.Context, chainID uint64, account common.Address, deadline *big.Int, pairs []Verdict) (recorded, cleared int, err error) {
	if len(pairs) > MaxDemandPerVote {
		return 0, 0, fmt.Errorf("store: a verdict names at most %d contracts", MaxDemandPerVote)
	}
	if deadline == nil || deadline.Sign() < 0 {
		return 0, 0, fmt.Errorf("store: deadline must be a non-negative integer")
	}
	for _, p := range pairs {
		if p.Weight != 1 && p.Weight != -1 {
			return 0, 0, fmt.Errorf("store: weight must be -1 or 1, got %d", p.Weight)
		}
	}
	salt, err := s.demandSalt(ctx)
	if err != nil {
		return 0, 0, err
	}
	voter := voterKey(salt, chainID, account)

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// The replay guard first: the row is only written when the deadline rises,
		// and a signature that does not raise it is refused before anything moves.
		// The deadline is a uint256 on the wire, so it travels as text into NUMERIC.
		ct, err := tx.Exec(ctx, `
			INSERT INTO account_verdicts (chain_id, voter, deadline, signed_at)
			VALUES ($1, $2, $3::numeric, now())
			ON CONFLICT (chain_id, voter) DO UPDATE
			SET deadline = EXCLUDED.deadline, signed_at = now()
			WHERE account_verdicts.deadline < EXCLUDED.deadline`,
			int64(chainID), voter, deadline.String())
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return ErrStaleVerdict
		}

		keep := make([][]byte, 0, len(pairs))
		seen := map[common.Address]bool{}
		for _, p := range pairs {
			if seen[p.Address] {
				continue
			}
			seen[p.Address] = true
			keep = append(keep, p.Address.Bytes())
		}
		ct, err = tx.Exec(ctx, `
			DELETE FROM asset_demand
			WHERE chain_id = $1 AND voter = $2 AND NOT (address = ANY($3))`,
			int64(chainID), voter, keep)
		if err != nil {
			return err
		}
		cleared = int(ct.RowsAffected())

		seen = map[common.Address]bool{}
		for _, p := range pairs {
			if seen[p.Address] {
				continue
			}
			seen[p.Address] = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO asset_demand (chain_id, address, voter, weight)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (chain_id, address, voter) DO UPDATE
				SET weight = EXCLUDED.weight, votes = asset_demand.votes + 1, last_at = now()`,
				int64(chainID), p.Address.Bytes(), voter, int16(p.Weight)); err != nil {
				return err
			}
			recorded++
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return recorded, cleared, nil
}
