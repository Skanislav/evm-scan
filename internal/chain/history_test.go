package chain

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// prunedNode serves logs only at or above floor, which is how a node that dropped
// ancient receipts behaves.
type prunedNode struct {
	floor uint64
	// probes counts Logs calls, so the search's cost can be asserted.
	probes int
	// deadAtHead makes even the head unservable, modelling a broken node.
	deadAtHead bool
	head       uint64
}

func (p *prunedNode) Logs(_ context.Context, q Query) ([]types.Log, error) {
	p.probes++
	if p.deadAtHead && q.From == p.head {
		return nil, errors.New("no backend")
	}
	if q.From < p.floor {
		return nil, errors.New("receipt not found")
	}
	return nil, nil
}

func (p *prunedNode) ChainID(context.Context) (uint64, error)   { return 1, nil }
func (p *prunedNode) HeadBlock(context.Context) (uint64, error) { return p.head, nil }
func (p *prunedNode) HeaderHash(context.Context, uint64) (common.Hash, error) {
	return common.Hash{}, nil
}
func (p *prunedNode) SubscribeLogs(context.Context, Query, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, ErrNotStreaming
}
func (p *prunedNode) CallAtHead(context.Context, ethereum.CallMsg) ([]byte, error) { return nil, nil }
func (p *prunedNode) CodeAt(context.Context, common.Address) ([]byte, error)       { return nil, nil }
func (p *prunedNode) Endpoint() Endpoint                                           { return Endpoint{} }
func (p *prunedNode) Close()                                                       {}

func TestHistoryFloorFindsThePruningBoundary(t *testing.T) {
	const head = 1_000_000
	for _, floor := range []uint64{1, 2, 4999, 500_000, 999_999} {
		node := &prunedNode{floor: floor, head: head}

		got, err := HistoryFloor(context.Background(), node, head)
		if err != nil {
			t.Fatalf("floor %d: %v", floor, err)
		}
		if got != floor {
			t.Errorf("floor %d: got %d", floor, got)
		}
	}
}

// TestHistoryFloorReportsZeroForFullHistory is the case that must stay cheap: a node
// with everything should be settled by the single probe at block 0.
func TestHistoryFloorReportsZeroForFullHistory(t *testing.T) {
	node := &prunedNode{floor: 0, head: 1_000_000}

	got, err := HistoryFloor(context.Background(), node, node.head)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("floor = %d, want 0", got)
	}
	if node.probes > 2 {
		t.Errorf("took %d probes for a full-history node, want at most 2", node.probes)
	}
}

// TestHistoryFloorIsLogarithmic guards against the search degrading into a linear
// walk, which on a mainnet-sized chain would be millions of RPC calls.
func TestHistoryFloorIsLogarithmic(t *testing.T) {
	node := &prunedNode{floor: 12_345_678, head: 21_000_000}

	if _, err := HistoryFloor(context.Background(), node, node.head); err != nil {
		t.Fatal(err)
	}
	if node.probes > 30 {
		t.Errorf("took %d probes, want O(log head)", node.probes)
	}
}

// TestHistoryFloorRejectsUnusableNode: without this check the binary search would
// return `head` and the caller would conclude it simply has no history, hiding a
// node that is actually broken.
func TestHistoryFloorRejectsUnusableNode(t *testing.T) {
	node := &prunedNode{floor: 0, head: 500, deadAtHead: true}

	if _, err := HistoryFloor(context.Background(), node, node.head); err == nil {
		t.Error("expected an error when the node cannot serve logs at the head")
	}
}

func TestHistoryFloorHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	node := &prunedNode{floor: 100, head: 1000}
	if _, err := HistoryFloor(ctx, node, node.head); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
