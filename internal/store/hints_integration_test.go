package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/store"
)

// A hint replaces an older one only when its deadline rises, so an old signature
// cannot put an old hint back; everything else round-trips.
func TestAccountHintDeadlineOnlyRises(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	key, _ := crypto.GenerateKey()
	account := crypto.PubkeyToAddress(key.PublicKey)

	if _, err := st.AccountHint(ctx, account); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fresh account: %v, want ErrNotFound", err)
	}
	first := store.AccountHint{Account: account, Bytes: []byte("XORF-1"), Digest: crypto.Keccak256Hash([]byte("XORF-1")), Deadline: 100}
	if err := st.PutAccountHint(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := st.AccountHint(ctx, account)
	if err != nil || string(got.Bytes) != "XORF-1" || got.Digest != first.Digest || got.Deadline != 100 || got.Account != account {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// Same deadline: not newer, refused.
	if err := st.PutAccountHint(ctx, first); !errors.Is(err, store.ErrStaleHint) {
		t.Fatalf("same deadline: %v, want ErrStaleHint", err)
	}
	// Older: refused, bytes untouched.
	older := store.AccountHint{Account: account, Bytes: []byte("XORF-0"), Digest: common.Hash{1}, Deadline: 50}
	if err := st.PutAccountHint(ctx, older); !errors.Is(err, store.ErrStaleHint) {
		t.Fatalf("older deadline: %v, want ErrStaleHint", err)
	}
	// Newer: replaces.
	newer := store.AccountHint{Account: account, Bytes: []byte("XORF-2"), Digest: common.Hash{2}, Deadline: 200}
	if err := st.PutAccountHint(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.AccountHint(ctx, account); string(got.Bytes) != "XORF-2" || got.Deadline != 200 {
		t.Fatalf("after newer = %+v", got)
	}
}
