package ens_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/ens"
)

// TestLookupAgainstMainnet resolves real names against the real registry.
//
// Skipped unless pointed at a mainnet endpoint, like the other node-backed tests
// here:
//
//	EVMSCAN_TEST_MAINNET=https://ethereum-rpc.publicnode.com \
//	  go test ./internal/ens/ -run Mainnet -v
//
// The hermetic tests pin the encoding against bytes captured from this call. This
// one is what notices if the registry itself moves underneath them.
func TestLookupAgainstMainnet(t *testing.T) {
	node := os.Getenv("EVMSCAN_TEST_MAINNET")
	if node == "" {
		t.Skip("set EVMSCAN_TEST_MAINNET to a mainnet endpoint to run this")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, err := chain.Dial(ctx, node, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer src.Close()

	r := ens.New(src, common.Address{})

	for label, want := range map[string]uint64{
		"ethereum": 1,
		"optimism": 10,
		"base":     8453,
		"arbitrum": 42161,
	} {
		c, err := r.Lookup(ctx, label)
		if err != nil {
			t.Errorf("Lookup(%q): %v", label, err)
			continue
		}
		if c.ChainID != want {
			t.Errorf("Lookup(%q) = chain %d, want %d", label, c.ChainID, want)
		}
		t.Logf("%-9s -> chain %-6d %s", c.Name, c.ChainID, c.URL)
	}

	// The fallback path's premise: an unregistered name is a quiet no. If this
	// ever starts resolving, the UI's manual-entry branch needs revisiting.
	if c, err := r.Lookup(ctx, "arc"); err == nil {
		t.Errorf("arc.on.eth now resolves to chain %d; the fallback path assumed it does not", c.ChainID)
	}
}
