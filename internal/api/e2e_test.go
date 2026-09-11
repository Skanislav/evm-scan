package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintfilter"
)

// TestEndToEndAgainstRealList builds a filter from a real published token list,
// serves it over HTTP, and reads it back the way a browser would. It is skipped
// without EVMSCAN_TEST_TOKENLIST, in the same spirit as the node-backed tests.
func TestEndToEndAgainstRealList(t *testing.T) {
	src := os.Getenv("EVMSCAN_TEST_TOKENLIST")
	if src == "" {
		t.Skip("set EVMSCAN_TEST_TOKENLIST to a tokenlists.org URL")
	}

	c, addrs, err := LoadTokenLists(context.Background(), 1, []string{src}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{
		Chains:         chainsWith(1),
		TokenFilters:   map[uint64]*hintfilter.Cache{1: c},
		TokenAddresses: map[uint64][]common.Address{1: addrs},
		Log:            slog.Default(),
	})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	f, err := hintfilter.FetchFilter(context.Background(), srv.URL+"/v1/hints/tokens-1.xorf")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("served %d tokens as %s", f.Count(), f.Structure)

	sub := hintfilter.Subkey(hintfilter.PublicSecret, 1, hintfilter.KindToken)
	for _, a := range addrs {
		if !f.Contains(hintfilter.TokenKey(sub, a)) {
			t.Fatalf("%s went missing between the builder and the wire", a)
		}
	}
	t.Logf("all %d listed tokens survived the round trip", len(addrs))
}
