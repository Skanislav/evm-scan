package chain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// nonceService answers eth_getTransactionCount the way Helios does: "latest" works,
// "pending" is not a block it knows. It records every tag it was asked for.
type nonceService struct {
	pendingErr string
	tags       []string
}

func (s *nonceService) GetTransactionCount(_ common.Address, block rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	tag := block.String()
	s.tags = append(s.tags, tag)
	if tag == "pending" {
		if s.pendingErr != "" {
			return 0, errors.New(s.pendingErr)
		}
		return 8, nil
	}
	return 7, nil
}

// inprocNode wires a Node to an in-process RPC server, so the test runs where no
// socket can be bound.
func inprocNode(t *testing.T, svc *nonceService) *Node {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", svc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	c := rpc.DialInProc(srv)
	t.Cleanup(c.Close)
	return &Node{rpc: c, eth: ethclient.NewClient(c), meter: NewMeter()}
}

func TestPendingNonceAtFallsBackToLatest(t *testing.T) {
	svc := &nonceService{pendingErr: "block not found"}
	n := inprocNode(t, svc)

	nonce, err := n.PendingNonceAt(context.Background(), common.Address{1})
	if err != nil {
		t.Fatalf("PendingNonceAt: %v", err)
	}
	if nonce != 7 {
		t.Fatalf("nonce = %d, want the latest nonce 7", nonce)
	}
	if got := strings.Join(svc.tags, ","); got != "pending,latest" {
		t.Fatalf("asked for %q, want pending then latest", got)
	}
}

func TestPendingNonceAtPrefersPending(t *testing.T) {
	svc := &nonceService{}
	n := inprocNode(t, svc)

	nonce, err := n.PendingNonceAt(context.Background(), common.Address{1})
	if err != nil {
		t.Fatalf("PendingNonceAt: %v", err)
	}
	if nonce != 8 {
		t.Fatalf("nonce = %d, want the pending nonce 8", nonce)
	}
	if got := strings.Join(svc.tags, ","); got != "pending" {
		t.Fatalf("asked for %q, want pending only", got)
	}
}
