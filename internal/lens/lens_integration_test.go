package lens_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/lens"
)

// TestQueryAgainstNode runs the deployless lens against a real node.
//
// The unit tests fake the node, so they prove the wire format and the batching but
// not the trick itself: that a node will execute creation code sent with no `to`
// address and hand the constructor's return value back. Only a real client settles
// that, so it is worth a test even though it cannot run in CI:
//
//	EVMSCAN_TEST_NODE=/tmp/devchain/geth.ipc \
//	EVMSCAN_TEST_ACCOUNT=0x… EVMSCAN_TEST_TOKEN=0x…,0x… \
//	go test ./internal/lens/ -run AgainstNode -v
func TestQueryAgainstNode(t *testing.T) {
	node := os.Getenv("EVMSCAN_TEST_NODE")
	account := os.Getenv("EVMSCAN_TEST_ACCOUNT")
	if node == "" || account == "" {
		t.Skip("set EVMSCAN_TEST_NODE and EVMSCAN_TEST_ACCOUNT to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, err := chain.Dial(ctx, node, false)
	if err != nil {
		t.Fatalf("dial %s: %v", node, err)
	}
	defer src.Close()

	req := lens.Request{
		Account:        common.HexToAddress(account),
		IncludeURI:     true,
		EnumerateLimit: 8,
	}
	for _, raw := range strings.Split(os.Getenv("EVMSCAN_TEST_TOKEN"), ",") {
		if raw = strings.TrimSpace(raw); raw != "" {
			req.Tokens = append(req.Tokens, lens.TokenQuery{Token: common.HexToAddress(raw)})
		}
	}
	if spender := os.Getenv("EVMSCAN_TEST_SPENDER"); spender != "" {
		req.Spenders = append(req.Spenders, common.HexToAddress(spender))
	}

	res, err := lens.Query(ctx, src, req)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	head, err := src.HeadBlock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The lens reports the block it actually ran on, which is the point of reading it
	// from inside the EVM rather than asking for the head separately.
	if res.BlockNumber == 0 || res.BlockNumber > head+1 {
		t.Errorf("block = %d, head = %d", res.BlockNumber, head)
	}
	if chainID, err := src.ChainID(ctx); err == nil && res.ChainID != chainID {
		t.Errorf("chain id = %d, node says %d", res.ChainID, chainID)
	}
	if res.Account.Balance == nil {
		t.Fatal("no native balance came back")
	}
	if !res.Account.NonceKnown {
		t.Error("nonce should have been filled from eth_getTransactionCount")
	}
	t.Logf("block %d chain %d: balance=%s nonce=%d contract=%v delegated=%v delegate=%s calls=%d atomic=%v",
		res.BlockNumber, res.ChainID, res.Account.Balance, res.Account.Nonce,
		res.Account.IsContract, res.Account.IsDelegated, res.Account.Delegate, res.Calls, res.Atomic)

	if len(res.Tokens) != len(req.Tokens) {
		t.Fatalf("asked about %d tokens, got %d back", len(req.Tokens), len(res.Tokens))
	}
	for i, tok := range res.Tokens {
		if tok.Address != req.Tokens[i].Token {
			t.Errorf("token %d is %s, asked for %s", i, tok.Address, req.Tokens[i].Token)
		}
		t.Logf("%s %s (%s) standard=%s decimals=%v balance=%v ids=%d",
			tok.Address, tok.Symbol, tok.Name, tok.Standard, tok.Decimals, tok.Balance, len(tok.IDs))
		for _, id := range tok.IDs {
			t.Logf("  id %s owner=%v balance=%v uri=%q", id.ID, id.Owner, id.Balance, id.URI)
		}
	}
}
