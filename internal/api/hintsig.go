package api

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// The signature a reader puts on their hint.
//
// EIP-712, because the wallet then shows the reader three named fields — their
// account, the digest of the bytes, the deadline — where personal_sign would show
// hex. The domain names no chain and no contract: the bloom is cross-chain by
// construction, nothing on chain verifies it, and a chain-less domain lets a
// self-hosted daemon accept a hint signed against the hosted one. What binds the
// signature to this purpose is the type name and the domain name; what binds it to
// a moment is the deadline, which the store keeps monotonic per account so an old
// signature cannot put an old hint back.
//
// The page builds the identical typed data by hand (hints.js, submitHint); there is
// no endpoint to fetch it from because nothing in it is deployment-specific.

var hintDomain = apitypes.TypedDataDomain{Name: "evm-scan hint", Version: "1"}

var hintTypes = apitypes.Types{
	"EIP712Domain": {{Name: "name", Type: "string"}, {Name: "version", Type: "string"}},
	"Hint": {
		{Name: "account", Type: "address"},
		{Name: "digest", Type: "bytes32"},
		{Name: "deadline", Type: "uint256"},
	},
}

// hintDigestToSign is the EIP-712 digest a wallet signs for a hint.
func hintDigestToSign(account common.Address, digest common.Hash, deadline *big.Int) ([]byte, error) {
	td := apitypes.TypedData{
		Types:       hintTypes,
		PrimaryType: "Hint",
		Domain:      hintDomain,
		Message: apitypes.TypedDataMessage{
			"account":  account.Hex(),
			"digest":   digest.Hex(),
			"deadline": (*math.HexOrDecimal256)(deadline),
		},
	}
	h, _, err := apitypes.TypedDataAndHash(td)
	return h, err
}

// recoverHintSigner returns who signed a hint. Wallets return v in {27, 28};
// go-ethereum recovers with v in {0, 1}, so the shift is undone here.
func recoverHintSigner(account common.Address, digest common.Hash, deadline *big.Int, sig []byte) (common.Address, error) {
	if len(sig) != 65 {
		return common.Address{}, errors.New("signature must be 65 bytes")
	}
	h, err := hintDigestToSign(account, digest, deadline)
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
