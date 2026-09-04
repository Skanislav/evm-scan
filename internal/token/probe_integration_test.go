package token_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/token"
)

// TestProbeAgainstNode exercises the metadata prober against a real node.
//
// Skipped unless EVMSCAN_TEST_NODE points at one, so `go test ./...` stays hermetic:
//
//	EVMSCAN_TEST_NODE=/tmp/devchain/geth.ipc \
//	EVMSCAN_TEST_TOKEN=0x… go test ./internal/token/ -run Probe -v
func TestProbeAgainstNode(t *testing.T) {
	node := os.Getenv("EVMSCAN_TEST_NODE")
	tokenAddr := os.Getenv("EVMSCAN_TEST_TOKEN")
	if node == "" || tokenAddr == "" {
		t.Skip("set EVMSCAN_TEST_NODE and EVMSCAN_TEST_TOKEN to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	src, err := chain.Dial(ctx, node, false)
	if err != nil {
		t.Fatalf("dial %s: %v", node, err)
	}
	defer src.Close()

	for _, addr := range strings.Split(tokenAddr, ",") {
		a := common.HexToAddress(strings.TrimSpace(addr))
		m := token.Probe(ctx, src, a)
		t.Logf("%s symbol=%q name=%q decimals=%v", a.Hex(), m.Symbol, m.Name, m.Decimals)
		if m.Symbol == "" && m.Name == "" {
			t.Errorf("%s: probe returned no metadata", a.Hex())
		}
	}
}
