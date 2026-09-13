package api

import (
	"bytes"
	"errors"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/Skanislav/evm-scan/internal/store"
)

// The signature a reader puts on their verdict.
//
// One EIP-712 message over the reader's whole split of their own holdings:
// recognized (+1) and not (-1), the rest absent. The wallet shows four named
// fields — account, chain, the digest of the pairs, the deadline — and the daemon
// recovers the signer from exactly that. The domain names no chain and no
// contract, for the same reason Hint's does not: nothing on chain verifies it, and
// a chain-less domain lets a self-hosted daemon accept a verdict signed against the
// hosted one. The chain the verdict is about is inside the message instead. What
// binds the signature to a moment is the deadline, which the store keeps monotonic
// per (chain, voter) so an old signature cannot put an old split back.
//
// The page builds the identical typed data by hand; internal/api/testdata/verdict.json
// pins the digest, the EIP-712 hash and a signature from a fixed key so the two
// implementations can be checked against one file.

var verdictDomain = apitypes.TypedDataDomain{Name: "evm-scan verdict", Version: "1"}

var verdictTypes = apitypes.Types{
	"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}},
	"Verdict": {
		{Name: "account", Type: "address"},
		{Name: "chainId", Type: "uint64"},
		{Name: "digest", Type: "bytes32"},
		{Name: "deadline", Type: "uint256"},
	},
}

// verdictDigest is keccak256 over the pairs sorted by address ascending, each as
// the 20-byte address followed by the weight as a two's-complement int8: 0x01 for
// +1, 0xff for -1. The caller's slice is not reordered. An empty list — retract
// everything — digests as keccak256 of nothing.
func verdictDigest(pairs []store.Verdict) common.Hash {
	sorted := make([]store.Verdict, len(pairs))
	copy(sorted, pairs)
	sort.SliceStable(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Address[:], sorted[j].Address[:]) < 0
	})
	buf := make([]byte, 0, 21*len(sorted))
	for _, p := range sorted {
		buf = append(buf, p.Address[:]...)
		buf = append(buf, byte(p.Weight))
	}
	return crypto.Keccak256Hash(buf)
}

// verdictDigestToSign is the EIP-712 digest a wallet signs for a verdict.
func verdictDigestToSign(account common.Address, chainID uint64, digest common.Hash, deadline *big.Int) ([]byte, error) {
	td := apitypes.TypedData{
		Types:       verdictTypes,
		PrimaryType: "Verdict",
		Domain:      verdictDomain,
		Message: apitypes.TypedDataMessage{
			"account":  account.Hex(),
			"chainId":  (*math.HexOrDecimal256)(new(big.Int).SetUint64(chainID)),
			"digest":   digest.Hex(),
			"deadline": (*math.HexOrDecimal256)(deadline),
		},
	}
	h, _, err := apitypes.TypedDataAndHash(td)
	return h, err
}

// recoverVerdictSigner returns who signed a verdict. Wallets return v in {27, 28};
// go-ethereum recovers with v in {0, 1}, so the shift is undone here.
func recoverVerdictSigner(account common.Address, chainID uint64, digest common.Hash, deadline *big.Int, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, errors.New("signature must be 65 bytes")
	}
	h, err := verdictDigestToSign(account, chainID, digest, deadline)
	if err != nil {
		return common.Address{}, err
	}
	raw := append([]byte{}, sig...)
	if raw[64] >= 27 {
		raw[64] -= 27
	}
	pub, err := crypto.SigToPub(h, raw)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}
