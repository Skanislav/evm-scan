package store

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// DeferPromotion survives restarts and applies to both observed and unseen
// contracts. Explicit operator promotion remains available during the cooldown.
func (s *Store) DeferPromotion(ctx context.Context, chainID uint64, addr common.Address, retryAfter time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO promotion_cooldowns (chain_id, address, retry_after) VALUES ($1, $2, $3)
		ON CONFLICT (chain_id, address) DO UPDATE SET retry_after = EXCLUDED.retry_after`,
		int64(chainID), addr.Bytes(), retryAfter)
	return err
}
