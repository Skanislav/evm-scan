package store

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

// Epoch lifecycle states.
const (
	EpochBuilt     = "built"     // computed locally, not yet on-chain
	EpochSubmitted = "submitted" // sent to the chain, receipt not yet seen
	EpochPublished = "published" // posted to HintRegistry, challenge window open
	EpochFinalized = "finalized"
	EpochRejected  = "rejected"
	EpochFailed    = "failed" // submitted but never confirmed
)

// Epoch is a commitment over the index as of ToBlock.
type Epoch struct {
	ID         int64
	ChainID    uint64
	FromBlock  uint64
	ToBlock    uint64
	MerkleRoot common.Hash
	// CoverageRoot commits to the per-asset block ranges this epoch stands behind
	// (see EpochCoverage). The registry pays coverage rewards against it.
	CoverageRoot common.Hash
	LeafCount    int64
	URI          string
	OnchainID    *int64
	TxHash       *common.Hash
	Status       string
	// SubmissionRef identifies an in-flight submission so a restart can resume
	// waiting for it instead of submitting again. For a plain transaction it is the
	// tx hash; other submitters may hand back something else.
	SubmissionRef *common.Hash
	SubmittedAt   *time.Time
	// ExpectedRewardWei is what the registry quoted for this epoch's coverage when
	// it was built. Empty when no registry was consulted.
	ExpectedRewardWei string
	// ClaimTx and RewardWei record the coverage claim after finalization. ClaimedAt
	// set with a nil ClaimTx means there was nothing worth claiming.
	ClaimTx   *common.Hash
	ClaimedAt *time.Time
	RewardWei string
}

// EpochCoverage is one asset's entry in a commitment's coverage tree: the block
// range the publisher scanned it over, which is what it gets paid for.
type EpochCoverage struct {
	Index       int
	Asset       common.Address
	RegistryKey common.Hash
	FromBlock   uint64
	ToBlock     uint64
	Leaf        common.Hash
}

// EpochLeaf is one account's entry in a commitment, retained so proofs can be served
// later without recomputing the index at that height.
type EpochLeaf struct {
	Index      int
	Account    common.Address
	AssetsHash common.Hash
	Leaf       common.Hash
}

const epochColumns = `id, chain_id, from_block, to_block, merkle_root, leaf_count, uri,
	onchain_id, tx_hash, status, submission_ref, submitted_at,
	COALESCE(coverage_root, ''::bytea), COALESCE(expected_reward_wei, ''), claim_tx, claimed_at,
	COALESCE(reward_wei, '')`

// CreateEpoch stores a commitment, its leaves and its coverage atomically.
func (s *Store) CreateEpoch(ctx context.Context, e Epoch, leaves []EpochLeaf, coverage []EpochCoverage) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO epochs (chain_id, from_block, to_block, merkle_root, leaf_count, uri, status,
			                    coverage_root, expected_reward_wei)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')) RETURNING id`,
			int64(e.ChainID), int64(e.FromBlock), int64(e.ToBlock), e.MerkleRoot.Bytes(),
			int64(len(leaves)), e.URI, EpochBuilt, e.CoverageRoot.Bytes(), e.ExpectedRewardWei).Scan(&id); err != nil {
			return err
		}

		b := &pgx.Batch{}
		for _, l := range leaves {
			b.Queue(`INSERT INTO epoch_leaves (epoch_id, idx, account, assets_hash, leaf)
			         VALUES ($1,$2,$3,$4,$5)`,
				id, l.Index, l.Account.Bytes(), l.AssetsHash.Bytes(), l.Leaf.Bytes())
		}
		for _, c := range coverage {
			b.Queue(`INSERT INTO epoch_coverage (epoch_id, idx, asset, registry_key, from_block, to_block, leaf)
			         VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				id, c.Index, c.Asset.Bytes(), c.RegistryKey.Bytes(), int64(c.FromBlock), int64(c.ToBlock), c.Leaf.Bytes())
		}
		br := tx.SendBatch(ctx, b)
		defer br.Close()
		for i := 0; i < len(leaves)+len(coverage); i++ {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// MarkClaimed records the outcome of a coverage claim. A zero tx hash means the
// epoch had nothing claimable and no transaction was sent.
func (s *Store) MarkClaimed(ctx context.Context, id int64, tx common.Hash, rewardWei string) error {
	var txBytes []byte
	if tx != (common.Hash{}) {
		txBytes = tx.Bytes()
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE epochs SET claim_tx = $2, claimed_at = now(), reward_wei = $3 WHERE id = $1`,
		id, txBytes, rewardWei)
	return err
}

// UnclaimedFinalized returns finalized commitments whose coverage reward has not
// been claimed, oldest first.
func (s *Store) UnclaimedFinalized(ctx context.Context, chainID uint64) ([]Epoch, error) {
	return s.queryEpochs(ctx, `
		SELECT `+epochColumns+` FROM epochs
		WHERE chain_id = $1 AND status = $2 AND onchain_id IS NOT NULL AND claimed_at IS NULL
		ORDER BY id`, int64(chainID), EpochFinalized)
}

// EpochCoverage returns a commitment's coverage leaves in tree order.
func (s *Store) EpochCoverage(ctx context.Context, epochID int64) ([]EpochCoverage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT idx, asset, registry_key, from_block, to_block, leaf FROM epoch_coverage
		 WHERE epoch_id = $1 ORDER BY idx`, epochID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EpochCoverage
	for rows.Next() {
		var (
			c          EpochCoverage
			asset, key []byte
			from, to   int64
			leaf       []byte
		)
		if err := rows.Scan(&c.Index, &asset, &key, &from, &to, &leaf); err != nil {
			return nil, err
		}
		c.Asset = common.BytesToAddress(asset)
		c.RegistryKey = common.BytesToHash(key)
		c.FromBlock, c.ToBlock = uint64(from), uint64(to)
		c.Leaf = common.BytesToHash(leaf)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkSubmitted records that a commitment has been handed to the chain, before its
// receipt is known. This is the write that makes resubmission unnecessary after a
// crash.
func (s *Store) MarkSubmitted(ctx context.Context, id int64, ref common.Hash) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE epochs SET status = $2, submission_ref = $3, submitted_at = now() WHERE id = $1`,
		id, EpochSubmitted, ref.Bytes())
	return err
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
	row := s.pool.QueryRow(ctx, `SELECT `+epochColumns+` FROM epochs WHERE id = $1`, id)
	e, err := scanEpoch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epoch{}, ErrNotFound
	}
	return e, err
}

// EpochByOnchainID finds the local commitment behind a registry epoch id.
func (s *Store) EpochByOnchainID(ctx context.Context, chainID uint64, onchainID int64) (Epoch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+epochColumns+` FROM epochs
		WHERE chain_id = $1 AND onchain_id = $2 ORDER BY id DESC LIMIT 1`, int64(chainID), onchainID)
	e, err := scanEpoch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epoch{}, ErrNotFound
	}
	return e, err
}

// LatestFinalizedEpoch is the newest commitment this store knows to be final.
func (s *Store) LatestFinalizedEpoch(ctx context.Context, chainID uint64) (Epoch, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+epochColumns+` FROM epochs
		WHERE chain_id = $1 AND status = $2 AND onchain_id IS NOT NULL
		ORDER BY onchain_id DESC LIMIT 1`, int64(chainID), EpochFinalized)
	e, err := scanEpoch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Epoch{}, ErrNotFound
	}
	return e, err
}

// ListEpochs returns commitments newest first, for a chain or all chains when 0.
func (s *Store) ListEpochs(ctx context.Context, chainID uint64, limit int) ([]Epoch, error) {
	return s.queryEpochs(ctx, `
		SELECT `+epochColumns+` FROM epochs WHERE ($1 = 0 OR chain_id = $1)
		ORDER BY id DESC LIMIT $2`, int64(chainID), limit)
}

// PendingSubmissions returns commitments that were handed to the chain but whose
// receipt has not been recorded, oldest first.
func (s *Store) PendingSubmissions(ctx context.Context, chainID uint64) ([]Epoch, error) {
	return s.queryEpochs(ctx, `
		SELECT `+epochColumns+` FROM epochs WHERE chain_id = $1 AND status = $2
		ORDER BY id`, int64(chainID), EpochSubmitted)
}

// PublishedUnfinalized returns commitments that are on-chain and still inside, or
// past, their challenge window as far as this store knows.
func (s *Store) PublishedUnfinalized(ctx context.Context, chainID uint64) ([]Epoch, error) {
	return s.queryEpochs(ctx, `
		SELECT `+epochColumns+` FROM epochs
		WHERE chain_id = $1 AND status = $2 AND onchain_id IS NOT NULL
		ORDER BY id`, int64(chainID), EpochPublished)
}

// LatestCommittedRoots are the index and coverage roots of the most recent
// commitment that has reached the chain, or is on its way there. Building another
// epoch with both unchanged would only cost a bond and say nothing new.
func (s *Store) LatestCommittedRoots(ctx context.Context, chainID uint64) (root, coverage common.Hash, ok bool, err error) {
	var r, c []byte
	err = s.pool.QueryRow(ctx, `
		SELECT merkle_root, COALESCE(coverage_root, ''::bytea) FROM epochs
		WHERE chain_id = $1 AND status IN ($2, $3, $4)
		ORDER BY id DESC LIMIT 1`,
		int64(chainID), EpochSubmitted, EpochPublished, EpochFinalized).Scan(&r, &c)
	if errors.Is(err, pgx.ErrNoRows) {
		return common.Hash{}, common.Hash{}, false, nil
	}
	if err != nil {
		return common.Hash{}, common.Hash{}, false, err
	}
	return common.BytesToHash(r), common.BytesToHash(c), true, nil
}

func (s *Store) queryEpochs(ctx context.Context, sql string, args ...any) ([]Epoch, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
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
		e           Epoch
		cid         int64
		fb, tb      int64
		root        []byte
		txHash      []byte
		ref         []byte
		submittedAt *time.Time
		covRoot     []byte
		claimTx     []byte
		claimedAt   *time.Time
	)
	if err := r.Scan(&e.ID, &cid, &fb, &tb, &root, &e.LeafCount, &e.URI,
		&e.OnchainID, &txHash, &e.Status, &ref, &submittedAt,
		&covRoot, &e.ExpectedRewardWei, &claimTx, &claimedAt, &e.RewardWei); err != nil {
		return Epoch{}, err
	}
	e.ChainID = uint64(cid)
	e.FromBlock, e.ToBlock = uint64(fb), uint64(tb)
	e.MerkleRoot = common.BytesToHash(root)
	if len(covRoot) > 0 {
		e.CoverageRoot = common.BytesToHash(covRoot)
	}
	if len(claimTx) > 0 {
		h := common.BytesToHash(claimTx)
		e.ClaimTx = &h
	}
	e.ClaimedAt = claimedAt
	if len(txHash) > 0 {
		h := common.BytesToHash(txHash)
		e.TxHash = &h
	}
	if len(ref) > 0 {
		h := common.BytesToHash(ref)
		e.SubmissionRef = &h
	}
	e.SubmittedAt = submittedAt
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
