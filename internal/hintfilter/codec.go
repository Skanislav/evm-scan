package hintfilter

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/FastFilter/xorfilter"
)

// The wire format.
//
// This has a Go writer and a JavaScript reader, and the two must agree byte for
// byte. That is the same discipline internal/merkle keeps against the Solidity
// verifier, for the same reason: two implementations of one format drift in
// silence, and the only thing that stops them is a shared fixture both sides must
// reproduce. See testdata/ and TestVectors.
//
// Everything is big-endian. The header is fixed-width up to the descriptor so a
// reader can find the descriptor's length without parsing anything variable first.
//
//	magic      "XORF"     4
//	version    uint8      1
//	flags      uint8      1   bit0: blinded
//	structure  uint8      1   1 = binary-fuse8, 2 = sorted-u64
//	chainID    uint64     8
//	kind       uint8      1
//	epochID    int64      8   -1 when not index-derived
//	toBlock    uint64     8   0 when not index-derived
//	descLen    uint16     2
//	desc       []byte     descLen
//	count      uint64     8
//
// then, for binary-fuse8:
//
//	seed               uint64  8
//	segmentLength      uint32  4
//	segmentLengthMask  uint32  4
//	segmentCount       uint32  4
//	segmentCountLength uint32  4
//	fingerprintLen     uint32  4
//	fingerprints       []uint8 fingerprintLen
//
// or, for sorted-u64:
//
//	keys  []uint64  count*8

const (
	magic   = "XORF"
	version = 1

	flagBlinded = 1 << 0

	headerFixed = 4 + 1 + 1 + 1 + 8 + 1 + 8 + 8 + 2
	// maxDesc bounds the descriptor so a malformed or hostile file cannot make a
	// reader allocate on a claimed length it never intends to supply.
	maxDesc = 8192
)

// ErrFormat is returned for anything that is not a well-formed .xorf file.
var ErrFormat = errors.New("hintfilter: malformed file")

// SaltDesc says how to re-derive the secret a blinded filter was built under.
//
// It records the method and its parameters and never the secret itself, which is
// what makes a blinded file self-describing: open one on a new device and it can
// tell you which passkey to tap without telling anyone what is inside it.
type SaltDesc struct {
	// KDF is "webauthn-prf", "eip191", "argon2id" or "pbkdf2".
	//
	// PBKDF2 is in this list because it is what the browser can actually do:
	// WebCrypto has no Argon2id and the page vendors no WASM to supply one. It is
	// markedly weaker against GPU attack, which is why the page says so where the
	// reader chooses rather than here — but a descriptor that could not name the KDF
	// its own builder used would make the file unopenable by the honest path while
	// doing nothing to the attacker, who does not read descriptors.
	KDF string `json:"kdf"`
	// CredID and Input are the WebAuthn credential and PRF input, base64url.
	CredID string `json:"cred_id,omitempty"`
	Input  string `json:"input,omitempty"`
	// Account is the address whose signature derives an eip191 secret.
	Account string `json:"account,omitempty"`
	// Message is the constant that was signed. It is deliberately a bare constant
	// with no origin in it: folding in the origin would stop another site
	// harvesting the same secret, but it would also make a filter built on one
	// deployment unreadable on a reader's own daemon, and self-hosting is the
	// point of this project. The reader is deriving a key, not authenticating a
	// session, so replay is not the threat being defended against here.
	Message string `json:"message,omitempty"`
	// Argon2id parameters. KDFSalt is Argon2's own salt parameter and has nothing
	// to do with the blinding secret; the two are never the same value and giving
	// them the same name is how that gets misread.
	M       uint32 `json:"m,omitempty"`
	T       uint32 `json:"t,omitempty"`
	P       uint8  `json:"p,omitempty"`
	KDFSalt string `json:"kdf_salt,omitempty"`
	// Iterations is PBKDF2's work factor, which Argon2's three parameters have no
	// place to hold. It is recorded rather than assumed so that raising the factor
	// later does not lock anyone out of a file built before the change.
	Iterations uint32 `json:"iterations,omitempty"`
}

// Encode writes the filter in the wire format.
//
// The same keys with the same metadata always produce the same bytes: the fuse
// builder seeds its retry loop from a fixed counter rather than from the clock or
// math/rand, and nothing else here varies. That is what lets a published filter be
// quoted by digest and rebuilt from its inputs by anyone who wants to check it. A
// file nobody can diff against its source is not an artifact, it is a blob.
func (f *Filter) Encode() ([]byte, error) {
	if len(f.Desc) > maxDesc {
		return nil, fmt.Errorf("hintfilter: descriptor is %d bytes, over the %d limit", len(f.Desc), maxDesc)
	}

	out := make([]byte, 0, headerFixed+len(f.Desc)+8+int(f.count)*8)
	out = append(out, magic...)
	out = append(out, version)
	var flags byte
	if f.Blinded {
		flags |= flagBlinded
	}
	out = append(out, flags, byte(f.Structure))
	out = binary.BigEndian.AppendUint64(out, f.ChainID)
	out = append(out, byte(f.Kind))
	out = binary.BigEndian.AppendUint64(out, uint64(f.EpochID))
	out = binary.BigEndian.AppendUint64(out, f.ToBlock)
	out = binary.BigEndian.AppendUint16(out, uint16(len(f.Desc)))
	out = append(out, f.Desc...)
	out = binary.BigEndian.AppendUint64(out, f.count)

	switch f.Structure {
	case StructureSortedU64:
		for _, k := range f.sorted {
			out = binary.BigEndian.AppendUint64(out, k)
		}
	case StructureFuse8:
		out = binary.BigEndian.AppendUint64(out, f.fuse.Seed)
		out = binary.BigEndian.AppendUint32(out, f.fuse.SegmentLength)
		out = binary.BigEndian.AppendUint32(out, f.fuse.SegmentLengthMask)
		out = binary.BigEndian.AppendUint32(out, f.fuse.SegmentCount)
		out = binary.BigEndian.AppendUint32(out, f.fuse.SegmentCountLength)
		out = binary.BigEndian.AppendUint32(out, uint32(len(f.fuse.Fingerprints)))
		out = append(out, f.fuse.Fingerprints...)
	default:
		return nil, fmt.Errorf("hintfilter: unknown structure %d", f.Structure)
	}
	return out, nil
}

// Decode parses a .xorf file.
func Decode(b []byte) (*Filter, error) {
	if len(b) < headerFixed {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a header", ErrFormat, len(b))
	}
	if string(b[:4]) != magic {
		return nil, fmt.Errorf("%w: bad magic %q", ErrFormat, b[:4])
	}
	if b[4] != version {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrFormat, b[4], version)
	}

	f := &Filter{}
	f.Blinded = b[5]&flagBlinded != 0
	f.Structure = Structure(b[6])
	f.ChainID = binary.BigEndian.Uint64(b[7:15])
	f.Kind = Kind(b[15])
	f.EpochID = int64(binary.BigEndian.Uint64(b[16:24]))
	f.ToBlock = binary.BigEndian.Uint64(b[24:32])
	descLen := int(binary.BigEndian.Uint16(b[32:34]))

	rest := b[headerFixed:]
	if len(rest) < descLen+8 {
		return nil, fmt.Errorf("%w: truncated in the descriptor", ErrFormat)
	}
	if descLen > 0 {
		f.Desc = append([]byte(nil), rest[:descLen]...)
	}
	rest = rest[descLen:]
	f.count = binary.BigEndian.Uint64(rest[:8])
	rest = rest[8:]

	switch f.Structure {
	case StructureSortedU64:
		// Check the claimed count against the bytes actually present before
		// allocating on it. A length prefix is an instruction to allocate, and a
		// reader that trusts one is a reader that can be made to allocate
		// anything.
		if uint64(len(rest)) != f.count*8 {
			return nil, fmt.Errorf("%w: %d key bytes for a claimed %d keys", ErrFormat, len(rest), f.count)
		}
		f.sorted = make([]uint64, f.count)
		for i := range f.sorted {
			f.sorted[i] = binary.BigEndian.Uint64(rest[i*8:])
		}
	case StructureFuse8:
		if len(rest) < 28 {
			return nil, fmt.Errorf("%w: truncated in the fuse header", ErrFormat)
		}
		fuse := &xorfilter.BinaryFuse[uint8]{}
		fuse.Seed = binary.BigEndian.Uint64(rest[0:8])
		fuse.SegmentLength = binary.BigEndian.Uint32(rest[8:12])
		fuse.SegmentLengthMask = binary.BigEndian.Uint32(rest[12:16])
		fuse.SegmentCount = binary.BigEndian.Uint32(rest[16:20])
		fuse.SegmentCountLength = binary.BigEndian.Uint32(rest[20:24])
		fpLen := binary.BigEndian.Uint32(rest[24:28])
		rest = rest[28:]
		if uint64(len(rest)) != uint64(fpLen) {
			return nil, fmt.Errorf("%w: %d fingerprint bytes for a claimed %d", ErrFormat, len(rest), fpLen)
		}
		fuse.Fingerprints = append([]uint8(nil), rest...)
		f.fuse = fuse
	default:
		return nil, fmt.Errorf("%w: unknown structure %d", ErrFormat, f.Structure)
	}
	return f, nil
}

// Descriptor parses the salt descriptor, if there is one.
func (f *Filter) Descriptor() (*SaltDesc, error) {
	if len(f.Desc) == 0 {
		return nil, nil
	}
	var d SaltDesc
	if err := json.Unmarshal(f.Desc, &d); err != nil {
		return nil, fmt.Errorf("hintfilter: descriptor: %w", err)
	}
	return &d, nil
}
