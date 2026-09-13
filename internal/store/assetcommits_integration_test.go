package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/store"
)

func TestAssetCommitReplacesOnlyWithLaterDeadline(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	account := crypto.PubkeyToAddress(key.PublicKey)
	a := common.HexToAddress("0x0000000000000000000000000000000000000001")
	b := common.HexToAddress("0x0000000000000000000000000000000000000002")
	first := store.AssetCommit{
		Account: account, Deadline: 100, Digest: crypto.Keccak256Hash([]byte("first")),
		Items: []store.AssetCommitItem{{ChainID: 8453, Asset: b}, {ChainID: 1, Asset: a}},
	}
	if err := st.PutAssetCommit(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := st.AssetCommit(ctx, account)
	if err != nil || got.Deadline != 100 || got.Digest != first.Digest || len(got.Items) != 2 || got.Items[0] != (store.AssetCommitItem{ChainID: 1, Asset: a}) || got.Items[1] != (store.AssetCommitItem{ChainID: 8453, Asset: b}) {
		t.Fatalf("first round trip = %+v, %v", got, err)
	}
	if err := st.PutAssetCommit(ctx, first); !errors.Is(err, store.ErrStaleAssetCommit) {
		t.Fatalf("same deadline = %v, want ErrStaleAssetCommit", err)
	}
	newer := store.AssetCommit{Account: account, Deadline: 101, Digest: crypto.Keccak256Hash([]byte("newer")), Items: []store.AssetCommitItem{{ChainID: 1, Asset: b}}}
	if err := st.PutAssetCommit(ctx, newer); err != nil {
		t.Fatal(err)
	}
	got, err = st.AssetCommit(ctx, account)
	if err != nil || got.Deadline != newer.Deadline || got.Digest != newer.Digest || len(got.Items) != 1 || got.Items[0] != newer.Items[0] {
		t.Fatalf("replacement = %+v, %v", got, err)
	}
}
