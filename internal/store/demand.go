package store

import (
	"context"
	"encoding/binary"
	"fmt"
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
	// Voters is the total: distinct API voters plus the on-chain count.
	Voters        uint64
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

const demandSelect = `
	SELECT t.chain_id, t.address, t.voters, COALESCE(o.voters, 0),
	       COALESCE(d.last_at, o.synced_at, now()),
	       (a.address IS NOT NULL), (c.address IS NOT NULL), (c.spam_at IS NOT NULL),
	       COALESCE(c.event_count, 0)
	FROM asset_demand_totals t
	LEFT JOIN asset_demand_onchain o USING (chain_id, address)
	LEFT JOIN (SELECT chain_id, address, MAX(last_at) AS last_at FROM asset_demand GROUP BY 1, 2) d USING (chain_id, address)
	LEFT JOIN assets a ON a.chain_id = t.chain_id AND a.address = t.address
	LEFT JOIN candidates c ON c.chain_id = t.chain_id AND c.address = t.address`

// ListDemand returns the most wanted contracts on a chain, most voters first.
func (s *Store) ListDemand(ctx context.Context, chainID uint64, limit int) ([]DemandRow, error) {
	rows, err := s.pool.Query(ctx, demandSelect+`
		WHERE t.chain_id = $1
		ORDER BY t.voters DESC, d.last_at DESC NULLS LAST
		LIMIT $2`, int64(chainID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemand(rows)
}

// DemandedUnseen returns voted contracts that discovery has never counted and the
// index does not keep, at or above minVoters. Discovery only counts what emitted an
// event inside its window, so a contract a wallet holds quietly can be wanted and
// still have no candidate row; this is how it reaches promotion.
func (s *Store) DemandedUnseen(ctx context.Context, chainID, minVoters uint64, limit int) ([]DemandRow, error) {
	rows, err := s.pool.Query(ctx, demandSelect+`
		WHERE t.chain_id = $1 AND t.voters >= $2 AND a.address IS NULL AND c.address IS NULL
		ORDER BY t.voters DESC
		LIMIT $3`, int64(chainID), int64(minVoters), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDemand(rows)
}

// DemandFor reports the total voters for each of addrs; absent means zero.
func (s *Store) DemandFor(ctx context.Context, chainID uint64, addrs []common.Address) (map[common.Address]uint64, error) {
	out := map[common.Address]uint64{}
	if len(addrs) == 0 {
		return out, nil
	}
	raw := make([][]byte, len(addrs))
	for i, a := range addrs {
		raw[i] = a.Bytes()
	}
	rows, err := s.pool.Query(ctx, `
		SELECT address, voters FROM asset_demand_totals
		WHERE chain_id = $1 AND address = ANY($2)`, int64(chainID), raw)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var addr []byte
		var n int64
		if err := rows.Scan(&addr, &n); err != nil {
			return nil, err
		}
		out[common.BytesToAddress(addr)] = uint64(n)
	}
	return out, rows.Err()
}

func scanDemand(rows pgx.Rows) ([]DemandRow, error) {
	var out []DemandRow
	for rows.Next() {
		var (
			r                DemandRow
			cid, voters, onc int64
			addr             []byte
			ev               int64
		)
		if err := rows.Scan(&cid, &addr, &voters, &onc, &r.LastAt, &r.Indexed, &r.Candidate, &r.Spam, &ev); err != nil {
			return nil, err
		}
		r.ChainID = uint64(cid)
		r.Address = common.BytesToAddress(addr)
		r.Voters = uint64(voters)
		r.OnchainVoters = uint64(onc)
		r.EventCount = uint64(ev)
		out = append(out, r)
	}
	return out, rows.Err()
}
