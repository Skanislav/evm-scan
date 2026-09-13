package api

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/store"
)

func TestAssetCommitSignatureRecovers(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	account := crypto.PubkeyToAddress(key.PublicKey)
	items := []store.AssetCommitItem{
		{ChainID: 8453, Asset: common.HexToAddress("0x0000000000000000000000000000000000000002")},
		{ChainID: 1, Asset: common.HexToAddress("0x0000000000000000000000000000000000000001")},
	}
	deadline := big.NewInt(1_800_000_000)
	digest := assetCommitDigest(items)
	h, err := assetCommitDigestToSign(account, digest, deadline)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := crypto.Sign(h, key)
	if err != nil {
		t.Fatal(err)
	}
	sig[64] += 27 // eth_signTypedData_v4 convention

	got, err := recoverAssetCommitSigner(account, digest, deadline, sig)
	if err != nil || got != account {
		t.Fatalf("recovered = %s, %v; want %s", got.Hex(), err, account.Hex())
	}
	changed := append([]store.AssetCommitItem(nil), items...)
	changed[0].ChainID = 1
	if got, err := recoverAssetCommitSigner(account, assetCommitDigest(changed), deadline, sig); err == nil && got == account {
		t.Fatal("signature over a different asset list recovered to the account")
	}
	if got, err := recoverAssetCommitSigner(account, digest, big.NewInt(1_800_000_001), sig); err == nil && got == account {
		t.Fatal("signature over another deadline recovered to the account")
	}
}

func TestAssetCommitDigestIgnoresInputOrder(t *testing.T) {
	a := store.AssetCommitItem{ChainID: 1, Asset: common.HexToAddress("0x0000000000000000000000000000000000000001")}
	b := store.AssetCommitItem{ChainID: 8453, Asset: common.HexToAddress("0x0000000000000000000000000000000000000002")}
	if assetCommitDigest([]store.AssetCommitItem{a, b}) != assetCommitDigest([]store.AssetCommitItem{b, a}) {
		t.Fatal("asset commit digest changed with input order")
	}
}
