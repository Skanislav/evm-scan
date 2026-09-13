package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/Skanislav/evm-scan/internal/userstate"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
)

var ErrStateConflict = errors.New("state changed; load, review and sign again")

// One account lock serializes both legacy paths with signed snapshot projection.
func lockState(ctx context.Context, tx pgx.Tx, a common.Address) error {
	h := crypto.Keccak256([]byte("evmscan/state/lock"), a[:])
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(h)))
	return err
}
func markLegacyState(ctx context.Context, tx pgx.Tx, a common.Address) error {
	_, err := tx.Exec(ctx, `UPDATE user_state_heads SET legacy_changed=true,generation=generation+1 WHERE account=$1`, a.Bytes())
	return err
}

type StateView struct {
	ID            common.Hash        `json:"id"`
	Snapshot      userstate.Snapshot `json:"snapshot"`
	Generation    string             `json:"generation"`
	LegacyChanged bool               `json:"legacy_changed"`
}

func (s *Store) UserState(ctx context.Context, a common.Address) (StateView, error) {
	var v StateView
	var id, raw []byte
	var gen int64
	err := s.pool.QueryRow(ctx, `SELECT h.id,r.snapshot,h.generation,h.legacy_changed FROM user_state_heads h JOIN user_state_revisions r ON r.id=h.id WHERE h.account=$1`, a.Bytes()).Scan(&id, &raw, &gen, &v.LegacyChanged)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.ID = common.BytesToHash(id)
	v.Generation = strconv.FormatInt(gen, 10)
	err = json.Unmarshal(raw, &v.Snapshot)
	return v, err
}
func (s *Store) StateRevision(ctx context.Context, id common.Hash) (userstate.Snapshot, error) {
	var v userstate.Snapshot
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT snapshot FROM user_state_revisions WHERE id=$1`, id.Bytes()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v)
	return v, err
}
func saveState(ctx context.Context, tx pgx.Tx, v userstate.Snapshot, id common.Hash, t *userstate.Trie) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO user_state_revisions(id,account,snapshot) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id.Bytes(), v.Account.Bytes(), raw); err != nil {
		return err
	}
	// Batch immutable nodes in bounded chunks instead of one network round trip per node.
	batch := &pgx.Batch{}
	flush := func() error { r := tx.SendBatch(ctx, batch); err := r.Close(); batch = &pgx.Batch{}; return err }
	for h, b := range t.Nodes {
		batch.Queue(`INSERT INTO user_state_nodes(hash,data) VALUES($1,$2) ON CONFLICT DO NOTHING`, h.Bytes(), b)
		if batch.Len() >= 1000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if batch.Len() > 0 {
		return flush()
	}
	return nil
}

// ImportState restores history only. It never changes a head or creates demand.
func (s *Store) ImportState(ctx context.Context, v userstate.Snapshot) error {
	id, t, err := v.Validate()
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error { return saveState(ctx, tx, v, id, t) })
}
func (s *Store) PutUserState(ctx context.Context, v userstate.Snapshot, generation string) error {
	id, t, err := v.Validate()
	if err != nil {
		return err
	}
	deadline, _ := userstate.Decimal(v.Deadline, 256)
	if !deadline.IsInt64() {
		return errors.New("admission deadline must fit unix seconds")
	}
	gen, err := strconv.ParseInt(generation, 10, 64)
	if err != nil || gen < 0 {
		return ErrStateConflict
	}
	salt, err := s.demandSalt(ctx)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockState(ctx, tx, v.Account); err != nil {
			return err
		}
		var oldID, raw []byte
		var oldGen int64
		err := tx.QueryRow(ctx, `SELECT h.id,r.snapshot,h.generation FROM user_state_heads h JOIN user_state_revisions r ON r.id=h.id WHERE h.account=$1`, v.Account.Bytes()).Scan(&oldID, &raw, &oldGen)
		if err == nil && common.BytesToHash(oldID) == id {
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if oldGen != gen {
			return ErrStateConflict
		}
		if deadline.Cmp(big.NewInt(time.Now().Unix())) <= 0 {
			return errors.New("state admission deadline expired")
		}
		if err == nil {
			var old userstate.Snapshot
			if err = json.Unmarshal(raw, &old); err != nil {
				return err
			}
			if !userstate.Successor(old, v) {
				return ErrStateConflict
			}
		} else if v.Revision != "1" {
			var prior []byte
			if err = tx.QueryRow(ctx, `SELECT snapshot FROM user_state_revisions WHERE id=$1`, v.Previous.Bytes()).Scan(&prior); err != nil {
				return ErrStateConflict
			}
			var old userstate.Snapshot
			if json.Unmarshal(prior, &old) != nil || !userstate.Successor(old, v) {
				return ErrStateConflict
			}
		}
		if err := saveState(ctx, tx, v, id, t); err != nil {
			return err
		}
		// Enumerate chain IDs, never deanonymize the legacy voter table. Once the user
		// opts in we can derive their voter key for each chain and replace their rows.
		rows, err := tx.Query(ctx, `SELECT DISTINCT chain_id FROM account_verdicts`)
		if err != nil {
			return err
		}
		chains := map[uint64]bool{}
		for rows.Next() {
			var c uint64
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return err
			}
			chains[c] = false
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, e := range v.Entries {
			if e.Kind == "verdict" {
				c, _ := strconv.ParseUint(e.ChainID, 10, 64)
				chains[c] = true
			}
		}
		for c, isNew := range chains {
			voter := voterKey(salt, c, v.Account)
			if !isNew {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_verdicts WHERE chain_id=$1 AND voter=$2)`, int64(c), voter).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					continue
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM asset_demand WHERE chain_id=$1 AND voter=$2`, int64(c), voter); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO account_verdicts(chain_id,voter,deadline,signed_at) VALUES($1,$2,$3::numeric,now()) ON CONFLICT(chain_id,voter) DO UPDATE SET deadline=GREATEST(account_verdicts.deadline,EXCLUDED.deadline),signed_at=now()`, int64(c), voter, v.Deadline); err != nil {
				return err
			}
		}
		var assets []AssetCommitItem
		for _, e := range v.Entries {
			c, _ := strconv.ParseUint(e.ChainID, 10, 64)
			if e.Kind == "asset" {
				assets = append(assets, AssetCommitItem{c, e.Address})
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO asset_demand(chain_id,address,voter,weight) VALUES($1,$2,$3,$4)`, int64(c), e.Address.Bytes(), voterKey(salt, c, v.Account), int16(e.Weight)); err != nil {
				return err
			}
		}
		// The legacy API retains its exact-list digest and replay floor.
		buf := assetCommitBytes(assets)
		if _, err := tx.Exec(ctx, `INSERT INTO account_asset_commits(account,digest,deadline,signed_at) VALUES($1,$2,$3,now()) ON CONFLICT(account) DO UPDATE SET digest=EXCLUDED.digest,deadline=GREATEST(account_asset_commits.deadline,EXCLUDED.deadline),signed_at=now()`, v.Account.Bytes(), crypto.Keccak256(buf), deadline.Int64()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM account_asset_commit_items WHERE account=$1`, v.Account.Bytes()); err != nil {
			return err
		}
		for _, a := range assets {
			if _, err := tx.Exec(ctx, `INSERT INTO account_asset_commit_items(account,chain_id,asset) VALUES($1,$2,$3)`, v.Account.Bytes(), int64(a.ChainID), a.Asset.Bytes()); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_state_heads(account,id,generation) VALUES($1,$2,1) ON CONFLICT(account) DO UPDATE SET id=EXCLUDED.id,generation=user_state_heads.generation+1,legacy_changed=false`, v.Account.Bytes(), id.Bytes())
		return err
	})
}
func assetCommitBytes(items []AssetCommitItem) []byte { // sorting must match the existing public asset-list signature
	sortAssets(items)
	var out []byte
	for _, a := range items {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], a.ChainID)
		out = append(out, c[:]...)
		out = append(out, a.Asset[:]...)
	}
	return out
}

// StateProjection is only available after public opt-in. It lets a reader review
// legacy writes before replacing them with a new signed snapshot.
func (s *Store) StateProjection(ctx context.Context, a common.Address) ([]userstate.Entry, error) {
	if _, err := s.UserState(ctx, a); err != nil {
		return nil, err
	}
	salt, err := s.demandSalt(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT chain_id FROM account_verdicts`)
	if err != nil {
		return nil, err
	}
	var chains []uint64
	for rows.Next() {
		var c uint64
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		chains = append(chains, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []userstate.Entry{}
	for _, c := range chains {
		rows, err := s.pool.Query(ctx, `SELECT address,weight FROM asset_demand WHERE chain_id=$1 AND voter=$2`, int64(c), voterKey(salt, c, a))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b []byte
			var w int8
			if err := rows.Scan(&b, &w); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, userstate.Entry{Kind: "verdict", ChainID: strconv.FormatUint(c, 10), Address: common.BytesToAddress(b), Weight: w})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	assets, err := s.AssetCommit(ctx, a)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, v := range assets.Items {
		out = append(out, userstate.Entry{Kind: "asset", ChainID: strconv.FormatUint(v.ChainID, 10), Address: v.Asset})
	}
	return out, nil
}
func (s *Store) BuildStateCheckpoint(ctx context.Context) (userstate.Checkpoint, error) {
	c := userstate.Checkpoint{Version: 1, Accounts: []userstate.AccountRevision{}, Snapshots: []userstate.Snapshot{}}
	rows, err := s.pool.Query(ctx, `SELECT h.account,h.id,r.snapshot FROM user_state_heads h JOIN user_state_revisions r ON r.id=h.id ORDER BY h.account`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, id, raw []byte
		if err := rows.Scan(&a, &id, &raw); err != nil {
			return c, err
		}
		var v userstate.Snapshot
		if err := json.Unmarshal(raw, &v); err != nil {
			return c, err
		}
		c.Accounts = append(c.Accounts, userstate.AccountRevision{Account: common.BytesToAddress(a), ID: common.BytesToHash(id)})
		c.Snapshots = append(c.Snapshots, v)
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	t, err := userstate.Aggregate(c.Accounts)
	if err != nil {
		return c, err
	}
	c.Root = t.Root
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO user_state_checkpoints(root,manifest) VALUES($1,$2) ON CONFLICT DO NOTHING`, c.Root.Bytes(), raw)
	return c, err
}
func (s *Store) StateCheckpoint(ctx context.Context, h common.Hash) (userstate.Checkpoint, error) {
	var c userstate.Checkpoint
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT manifest FROM user_state_checkpoints WHERE root=$1`, h.Bytes()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}
func (s *Store) ImportCheckpoint(ctx context.Context, c userstate.Checkpoint) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		for _, v := range c.Snapshots {
			id, t, err := v.Validate()
			if err != nil {
				return err
			}
			if err := saveState(ctx, tx, v, id, t); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_state_checkpoints(root,manifest) VALUES($1,$2) ON CONFLICT DO NOTHING`, c.Root.Bytes(), raw)
		return err
	})
}

func sortAssets(a []AssetCommitItem) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].ChainID != a[j].ChainID {
			return a[i].ChainID < a[j].ChainID
		}
		return bytes.Compare(a[i].Asset[:], a[j].Asset[:]) < 0
	})
}
