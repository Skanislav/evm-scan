// Package hintfilter builds and reads .xorf files: compact, publishable membership
// filters over the addresses an indexer or a reader cares about.
//
// Two things motivate the format. The first is narrowing. Asking "what does this
// account hold?" means choosing which contracts to read, and a reader cannot
// balanceOf a fifty-thousand-entry token list. A filter answers membership in about
// a byte per key, so the choice can be made locally, offline, against a file that
// was downloaded once.
//
// The second is that doing it locally is the only way to do it privately. Every
// other path in this repo — the HTTP API, the ERC-3668 gateway — puts the account
// in a URL or in calldata, which gives unforgeability but never unobservability:
// the operator learns exactly who asked, every time. A filter the reader already
// holds is queried without telling anyone what was asked.
//
// Keys can be blinded under a secret the reader holds (see Subkey). A filter can
// be tested but never enumerated, so blinding inverts cleanly: the holder reads
// their own set by walking a public token list and testing each entry, and everyone
// else holds the same file and the same list and learns nothing from either.
package hintfilter

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// domain separates this derivation from every other use of the same secret. A
// reader's passkey or wallet key will be used for other things, and a subkey minted
// here must not be usable anywhere else — or, more importantly, the other way round.
const domain = "evmscan/xorf/v1"

// Kind says what the keys in a filter are, and is mixed into the subkey so that a
// filter built over one kind cannot be tested with the subkey for another.
type Kind uint8

const (
	// KindToken keys a single contract address: "is this a known token?"
	KindToken Kind = 1
	// KindAccountToken keys an (account, token) pair: "did this account touch this
	// contract?" This is the one that narrows a portfolio read.
	KindAccountToken Kind = 2
)

func (k Kind) String() string {
	switch k {
	case KindToken:
		return "token"
	case KindAccountToken:
		return "account-token"
	}
	return "unknown"
}

// PublicSecret is the secret for a filter that is meant to be readable by anyone:
// a token list, or an index's own pairs. Nothing is hidden in that case, so the
// derivation still runs but with nothing secret in it, and the same code path
// serves both modes.
var PublicSecret = []byte{}

// Subkey derives the per-(chain, kind) key material.
//
// The reader's root secret is hashed here and never again, which does two things.
// It bounds the blast radius of the secret to one function, and it makes the
// derivation indifferent to the secret's length — a WebAuthn PRF output, a keccak
// of a wallet signature and an Argon2id output are all just bytes by the time they
// arrive, and none of the three has to agree with the others about size.
func Subkey(secret []byte, chainID uint64, kind Kind) [32]byte {
	var buf []byte
	buf = append(buf, secret...)
	buf = append(buf, domain...)
	buf = binary.BigEndian.AppendUint64(buf, chainID)
	buf = append(buf, byte(kind))
	return crypto.Keccak256Hash(buf)
}

// Key derives the 64-bit filter key for a value under a subkey.
//
// Truncating keccak to 64 bits is safe at the sizes this format is for: two million
// keys collide with probability around 1e-7, and a collision presents as a false
// positive, which callers already have to tolerate and which the live read that
// follows resolves anyway.
func Key(subkey [32]byte, parts ...[]byte) uint64 {
	buf := make([]byte, 0, 32+20*len(parts))
	buf = append(buf, subkey[:]...)
	for _, p := range parts {
		buf = append(buf, p...)
	}
	sum := crypto.Keccak256(buf)
	return binary.BigEndian.Uint64(sum[:8])
}

// TokenKey is Key for a KindToken filter.
func TokenKey(subkey [32]byte, token common.Address) uint64 {
	return Key(subkey, token[:])
}

// PairKey is Key for a KindAccountToken filter. Order matters and is fixed here so
// that a builder and a reader cannot disagree about it silently.
func PairKey(subkey [32]byte, account, token common.Address) uint64 {
	return Key(subkey, account[:], token[:])
}
