package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/Skanislav/evm-scan/internal/store"
)

// AssetCommit(account, digest, deadline) signs an exact cross-chain snapshot of
// contracts a wallet sweep found. The chain lives in each digested pair, rather than
// in the domain, so one signature can cover the whole cross-chain result.
var assetCommitDomain = apitypes.TypedDataDomain{Name: "evm-scan assets", Version: "1"}

var assetCommitTypes = apitypes.Types{
	"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}},
	"AssetCommit": {
		{Name: "account", Type: "address"},
		{Name: "digest", Type: "bytes32"},
		{Name: "deadline", Type: "uint256"},
	},
}

// assetCommitDigest is keccak256 over pairs sorted by chain then address. Each pair
// is chain_id as eight big-endian bytes followed by its 20-byte address. It does not
// reorder the caller's list, because the same list is later persisted for display.
func assetCommitDigest(items []store.AssetCommitItem) common.Hash {
	sorted := append([]store.AssetCommitItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ChainID != sorted[j].ChainID {
			return sorted[i].ChainID < sorted[j].ChainID
		}
		return bytes.Compare(sorted[i].Asset[:], sorted[j].Asset[:]) < 0
	})
	buf := make([]byte, 0, 28*len(sorted))
	var chain [8]byte
	for _, item := range sorted {
		binary.BigEndian.PutUint64(chain[:], item.ChainID)
		buf = append(buf, chain[:]...)
		buf = append(buf, item.Asset.Bytes()...)
	}
	return crypto.Keccak256Hash(buf)
}

func assetCommitDigestToSign(account common.Address, digest common.Hash, deadline *big.Int) ([]byte, error) {
	td := apitypes.TypedData{
		Types:       assetCommitTypes,
		PrimaryType: "AssetCommit",
		Domain:      assetCommitDomain,
		Message: apitypes.TypedDataMessage{
			"account":  account.Hex(),
			"digest":   digest.Hex(),
			"deadline": (*math.HexOrDecimal256)(deadline),
		},
	}
	h, _, err := apitypes.TypedDataAndHash(td)
	return h, err
}

func recoverAssetCommitSigner(account common.Address, digest common.Hash, deadline *big.Int, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, errors.New("signature must be 65 bytes")
	}
	h, err := assetCommitDigestToSign(account, digest, deadline)
	if err != nil {
		return common.Address{}, err
	}
	copySig := append([]byte(nil), sig...)
	if copySig[64] == 27 || copySig[64] == 28 {
		copySig[64] -= 27
	}
	pub, err := crypto.SigToPub(h, copySig)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}
