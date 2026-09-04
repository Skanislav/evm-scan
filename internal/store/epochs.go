package store

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Epoch lifecycle states.
const (
	EpochBuilt     = "built"     // computed locally, not yet on-chain
	EpochPublished = "published" // posted to HintRegistry, challenge window open
	EpochFinalized = "finalized"
	EpochRejected  = "rejected"
)

// Epoch is a commitment over the index as of ToBlock.
type Epoch struct {
	ID         int64
	ChainID    uint64
	FromBlock  uint64
	ToBlock    uint64
	MerkleRoot common.Hash
	LeafCount  int64
	URI        string
	OnchainID  *int64
	TxHash     *common.Hash
	Status     string
}

// EpochLeaf is one account's entry in a commitment, retained so proofs can be served
// later without recomputing the index at that height.
type EpochLeaf struct {
	Index      int
	Account    common.Address
	AssetsHash common.Hash
	Leaf       common.Hash
}

// CreateEpoch stores a commitment and its leaves atomically.
func (s *Store) CreateEpoch(ctx context.Context, e Epoch, leaves []EpochLeaf) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO epochs (chain_id, from_block, to_block, merkle_root, leaf_count, uri, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			int64(e.ChainID), int64(e.FromBlock), int64(e.ToBlock), e.MerkleRoot.Bytes(),
			int64(len(leaves)), e.URI, EpochBuilt).Scan(&id); err != nil {
			return err
		}

		b := &pgx.Batch{}
		for _, l := range leaves {
			b.Queue(`INSERT INTO epoch_leaves (epoch_id, idx, account, assets_hash, leaf)
			         VALUES ($1,$2,$3,$4,$5)`,
				id, l.Index, l.Account.Bytes(), l.AssetsHash.Bytes(), l.Leaf.Bytes())
		}
		br := tx.SendBatch(ctx, b)
		defer br.Close()
		for range leaves {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// MarkPublished records the on-chain identity of a commitment.
func (s *Store) MarkPublished(ctx context.Context, id int64, onchainID int64, txHash common.Hash) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE epochs SET status = $2, onchain_id = $3, tx_hash = $4 WHERE id = $1`,
		id, EpochPublished, onchainID, txHash.Bytes())
	return err
}

// SetEpochStatus updates a commitment's lifecycle state.
func (s *Store) SetEpochStatus(ctx context.Context, id int64, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE epochs SET status = $2 WHERE id = $1`, id, status)
	return err
}

func (s *Store) GetEpoch(ctx context.Context, id int64) (Epoch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, chain_id, from_block, to_block, merkle_root, leaf_count, uri,
		       onchain_id, tx_hash, status
		FROM epochs WHERE id = $1`, id)
	e, err := scanEpoch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epoch{}, ErrNotFound
	}
	return e, err
}

// ListEpochs returns commitments newest first, for a chain or all chains when 0.
func (s *Store) ListEpochs(ctx context.Context, chainID uint64, limit int) ([]Epoch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, chain_id, from_block, to_block, merkle_root, leaf_count, uri,
		       onchain_id, tx_hash, status
		FROM epochs WHERE ($1 = 0 OR chain_id = $1)
		ORDER BY id DESC LIMIT $2`, int64(chainID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Epoch
	for rows.Next() {
		e, err := scanEpoch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEpoch(r scannable) (Epoch, error) {
	var (
		e      Epoch
		cid    int64
		fb, tb int64
		root   []byte
		txHash []byte
	)
	if err := r.Scan(&e.ID, &cid, &fb, &tb, &root, &e.LeafCount, &e.URI,
		&e.OnchainID, &txHash, &e.Status); err != nil {
		return Epoch{}, err
	}
	e.ChainID = uint64(cid)
	e.FromBlock, e.ToBlock = uint64(fb), uint64(tb)
	e.MerkleRoot = common.BytesToHash(root)
	if len(txHash) > 0 {
		h := common.BytesToHash(txHash)
		e.TxHash = &h
	}
	return e, nil
}

// EpochLeaves returns every leaf of a commitment in tree order, which is what the
// proof builder needs to reconstruct the tree.
func (s *Store) EpochLeaves(ctx context.Context, epochID int64) ([]EpochLeaf, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT idx, account, assets_hash, leaf FROM epoch_leaves WHERE epoch_id = $1 ORDER BY idx`,
		epochID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EpochLeaf
	for rows.Next() {
		var (
			l                   EpochLeaf
			acc, ahash, leafRaw []byte
		)
		if err := rows.Scan(&l.Index, &acc, &ahash, &leafRaw); err != nil {
			return nil, err
		}
		l.Account = common.BytesToAddress(acc)
		l.AssetsHash = common.BytesToHash(ahash)
		l.Leaf = common.BytesToHash(leafRaw)
		out = append(out, l)
	}
	return out, rows.Err()
}

// EpochLeafFor looks up one account's leaf in a commitment.
func (s *Store) EpochLeafFor(ctx context.Context, epochID int64, account common.Address) (EpochLeaf, error) {
	var (
		l                   EpochLeaf
		acc, ahash, leafRaw []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT idx, account, assets_hash, leaf FROM epoch_leaves WHERE epoch_id = $1 AND account = $2`,
		epochID, account.Bytes()).Scan(&l.Index, &acc, &ahash, &leafRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return EpochLeaf{}, ErrNotFound
	}
	if err != nil {
		return EpochLeaf{}, err
	}
	l.Account = common.BytesToAddress(acc)
	l.AssetsHash = common.BytesToHash(ahash)
	l.Leaf = common.BytesToHash(leafRaw)
	return l, nil
}

// RegistrySyncCursor reports how far the HintRegistry mirror has read.
func (s *Store) RegistrySyncCursor(ctx context.Context, chainID uint64, addr common.Address) (uint64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT last_block FROM registry_sync WHERE registry_chain_id = $1 AND registry_address = $2`,
		int64(chainID), addr.Bytes()).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return uint64(n), err
}

// SetRegistrySyncCursor advances the HintRegistry mirror.
func (s *Store) SetRegistrySyncCursor(ctx context.Context, chainID uint64, addr common.Address, block uint64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO registry_sync (registry_chain_id, registry_address, last_block)
		VALUES ($1,$2,$3)
		ON CONFLICT (registry_chain_id, registry_address)
		DO UPDATE SET last_block = EXCLUDED.last_block, updated_at = now()`,
		int64(chainID), addr.Bytes(), int64(block))
	return err
}
