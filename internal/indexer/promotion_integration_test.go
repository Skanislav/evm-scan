package indexer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/store"
)

type promotionNode struct {
	chain.Source
	code  map[common.Address][]byte
	calls map[common.Address]int
	err   error
}

func (n *promotionNode) HeadBlock(context.Context) (uint64, error) { return 100, nil }
func (n *promotionNode) CodeAt(_ context.Context, addr common.Address) ([]byte, error) {
	n.calls[addr]++
	return n.code[addr], n.err
}
func (n *promotionNode) CallAtHead(context.Context, ethereum.CallMsg) ([]byte, error) {
	return nil, errors.New("metadata unavailable")
}

// Exercise the real queues with a one-attempt budget: an invalid address must
// stop occupying that slot, including after a restart, yet be retryable later.
func TestPromotionNoCodeCooldown(t *testing.T) {
	dsn := os.Getenv("EVMSCAN_TEST_DSN")
	if dsn == "" {
		t.Skip("set EVMSCAN_TEST_DSN to a scratch Postgres")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	for _, observed := range []bool{false, true} {
		name := "unseen"
		if observed {
			name = "candidate"
		}
		t.Run(name, func(t *testing.T) {
			chainID := uint64(1_000_000_000 + time.Now().UnixNano()%100_000_000)
			bad, good := common.Address{19: 1}, common.Address{19: 2}
			for _, voter := range []common.Address{{19: 3}, {19: 4}} {
				if _, err := st.RecordDemand(ctx, chainID, voter, []common.Address{bad}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.RecordDemand(ctx, chainID, common.Address{19: 3}, []common.Address{good}); err != nil {
				t.Fatal(err)
			}
			if observed {
				if err := st.UpsertCandidates(ctx, chainID, []store.Candidate{
					{Address: bad, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 2, EventCount: 2, BlocksSeen: 2},
					{Address: good, Standard: 20, FirstSeenBlock: 1, LastSeenBlock: 2, EventCount: 2, BlocksSeen: 2},
				}); err != nil {
					t.Fatal(err)
				}
			}
			node := &promotionNode{code: map[common.Address][]byte{good: {1}}, calls: map[common.Address]int{}}
			newService := func() *Service {
				return New(node, st, chainID, Options{Discovery: DiscoveryOptions{MinVoters: 1, MaxPromotionsPerTick: 1}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}
			svc := newService()
			if err := svc.autoPromote(ctx); err != nil {
				t.Fatal(err)
			}
			if node.calls[bad] != 1 || node.calls[good] != 0 {
				t.Fatalf("first tick: %v", node.calls)
			}
			// A new service has no memory of the previous failure.
			svc = newService()
			for range 3 {
				if err := svc.autoPromote(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if node.calls[bad] != 1 || node.calls[good] != 1 {
				t.Fatalf("cooldown did not free the queue: %v", node.calls)
			}
			cursors, err := st.ListCursors(ctx, chainID)
			if err != nil || len(cursors) != 1 || cursors[0].Address != good {
				t.Fatalf("valid asset not indexed: %v, %v", cursors, err)
			}
			var retryAfter time.Time
			if err := st.Pool().QueryRow(ctx, `SELECT retry_after FROM promotion_cooldowns WHERE chain_id=$1 AND address=$2`, int64(chainID), bad.Bytes()).Scan(&retryAfter); err != nil {
				t.Fatal(err)
			}
			if time.Until(retryAfter) < 23*time.Hour || time.Until(retryAfter) > 24*time.Hour {
				t.Fatalf("unexpected cooldown: %v", retryAfter)
			}
			// Expire without sleeping. A transport error must not refresh the cooldown.
			if err := st.DeferPromotion(ctx, chainID, bad, time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			node.err = errors.New("RPC temporarily unavailable")
			for range 2 {
				if err := svc.autoPromote(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if node.calls[bad] != 3 {
				t.Fatalf("RPC failure suppressed retry: %v", node.calls)
			}
			node.err = nil
			node.code[bad] = []byte{1}
			if err := svc.autoPromote(ctx); err != nil {
				t.Fatal(err)
			}
			cursors, err = st.ListCursors(ctx, chainID)
			if err != nil || len(cursors) != 2 {
				t.Fatalf("later deployment was not indexed: %v, %v", cursors, err)
			}
		})
	}
}
