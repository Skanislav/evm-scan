package store

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
)

type StatePublication struct {
	ID       int64          `json:"id"`
	Root     common.Hash    `json:"root"`
	Resolver common.Address `json:"resolver"`
	Node     common.Hash    `json:"node"`
	Hash     common.Hash    `json:"tx_hash"`
	Raw      []byte         `json:"-"`
	Status   string         `json:"status"`
	Error    string         `json:"error"`
}

func (s *Store) StatePublications(ctx context.Context) ([]StatePublication, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,root,resolver,node,tx_hash,raw_tx,status,error FROM user_state_publications ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatePublication{}
	for rows.Next() {
		var p StatePublication
		var root, resolver, node, hash []byte
		if err := rows.Scan(&p.ID, &root, &resolver, &node, &hash, &p.Raw, &p.Status, &p.Error); err != nil {
			return nil, err
		}
		p.Root = common.BytesToHash(root)
		p.Resolver = common.BytesToAddress(resolver)
		p.Node = common.BytesToHash(node)
		p.Hash = common.BytesToHash(hash)
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) SaveStatePublication(ctx context.Context, p StatePublication) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO user_state_publications(root,resolver,node,raw_tx,tx_hash) VALUES($1,$2,$3,$4,$5) RETURNING id`, p.Root.Bytes(), p.Resolver.Bytes(), p.Node.Bytes(), p.Raw, p.Hash.Bytes()).Scan(&id)
	return id, err
}
func (s *Store) UpdateStatePublication(ctx context.Context, id int64, status, message string) error {
	if status != "pending" && status != "failed" && status != "confirmed" {
		return errors.New("invalid publication status")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE user_state_publications SET status=$2,error=$3 WHERE id=$1`, id, status, message)
	if err == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}
