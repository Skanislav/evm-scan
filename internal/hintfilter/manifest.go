package hintfilter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// Manifest is what a release quotes; the .xorf is what a client downloads.
//
// It exists so that a published filter can be checked rather than trusted. Both
// digests are over the encoded file: sha256 because that is what a release note or
// a package manager will compare, keccak256 because that is what would go on chain
// if the digest is ever committed next to an epoch root.
type Manifest struct {
	Source        string `json:"source"`
	SourceVersion string `json:"source_version,omitempty"`
	BuiltAt       string `json:"built_at"`
	ChainID       uint64 `json:"chain_id"`
	KeyKind       string `json:"key_kind"`
	Structure     string `json:"structure"`
	Count         uint64 `json:"count"`
	Blinded       bool   `json:"blinded"`
	// EpochID and ToBlock say how current an index-derived filter is. A reader that
	// does not surface ToBlock is showing a portfolio as of an unstated block,
	// which is the difference between "you hold nothing" and "nothing had been
	// indexed yet when this was built".
	EpochID   int64  `json:"epoch_id,omitempty"`
	ToBlock   uint64 `json:"to_block,omitempty"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Keccak256 string `json:"keccak256"`
}

// BuildManifest describes an encoded filter.
func BuildManifest(f *Filter, encoded []byte, source, sourceVersion string) Manifest {
	sum := sha256.Sum256(encoded)
	return Manifest{
		Source:        source,
		SourceVersion: sourceVersion,
		BuiltAt:       time.Now().UTC().Format(time.RFC3339),
		ChainID:       f.ChainID,
		KeyKind:       f.Kind.String(),
		Structure:     f.Structure.String(),
		Count:         f.Count(),
		Blinded:       f.Blinded,
		EpochID:       f.EpochID,
		ToBlock:       f.ToBlock,
		Bytes:         len(encoded),
		SHA256:        "0x" + hex.EncodeToString(sum[:]),
		Keccak256:     crypto.Keccak256Hash(encoded).Hex(),
	}
}

func (m Manifest) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
