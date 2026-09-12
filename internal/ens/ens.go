// Package ens resolves chain names through ENS's on.eth chain registry.
//
// Adding a network otherwise means typing its chain id from memory, which is a
// number nobody remembers and which fails silently when it is wrong. `base.on.eth`
// answers with 8453, and the answer comes from the chain rather than from the
// operator.
//
// Three things about how it resolves are worth stating, because all three were
// measured rather than assumed and all three are load-bearing:
//
//   - It is onchain. The chain resolver answers directly; there is no ERC-3668
//     OffchainLookup and no gateway, so this is one eth_call at head like every
//     other node operation in the codebase. (internal/ccip is the client if that
//     ever changes; it is deliberately not wired in on speculation.)
//   - An unregistered name returns empty bytes rather than reverting, so "is this
//     chain known" is a cheap question with a boring answer.
//   - The registry carries identity and nothing operational: a name, a website, an
//     icon. There is no RPC record, no native-currency record, no explorer record.
//     That is the right shape — the endpoint is where the API key lives and has no
//     business in a public registry — and it is why resolving a name here never
//     says anything about the node that will be dialled. See docs/MULTICHAIN.md §3.
package ens

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/token"
)

// ChainResolver is the on.eth chain resolver on Ethereum mainnet.
//
// Same posture as internal/price/defaults.go: a well-known deployment, overridable
// in config, and echoed in every response that used it so a caller can check it
// against the one they trust. If the ENS DAO moves it, this is a one-line change
// and an override in the meantime.
var ChainResolver = common.HexToAddress("0x2a9B5787207863cf2d63d20172ed1F7bB2c9487A")

// Parent is the name chains live under.
const Parent = "on.eth"

// Function selectors.
var (
	selResolve = selector("resolve(bytes,bytes)") // ENSIP-10 wildcard resolution
	selData    = selector("data(bytes32,string)") // ENSIP-24 binary data records
	selText    = selector("text(bytes32,string)") // ENSIP-5 text records
)

// KeyInteropAddress holds the chain's ERC-7930 interoperable address, which is
// where its chain id comes from.
const KeyInteropAddress = "interoperable-address"

func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

// Chain is what the registry knows about a network.
type Chain struct {
	// Label is the bare name ("base"); Name is the full one ("base.on.eth").
	Label string
	Name  string
	// ChainID comes from the interoperable-address record. It is the only field
	// here that is not decoration.
	ChainID uint64
	// URL and Avatar are display, and best-effort in the same way token.Probe is:
	// a missing record is a blank field rather than an error.
	URL    string
	Avatar string
	// Resolver is the contract that answered, echoed so a caller can check it.
	Resolver common.Address
}

// ErrNotRegistered means the name resolved to nothing. It is an ordinary answer,
// not a failure: most chains are not in the registry yet, and the caller's job is
// to fall back to asking the operator rather than to report a problem.
var ErrNotRegistered = errors.New("ens: name is not registered under " + Parent)

// Resolver reads chain names. The zero value is not usable; call New.
type Resolver struct {
	src  chain.Source
	addr common.Address
}

// New builds a resolver over a mainnet source. Passing the zero address uses the
// well-known deployment.
func New(src chain.Source, resolver common.Address) *Resolver {
	if resolver == (common.Address{}) {
		resolver = ChainResolver
	}
	return &Resolver{src: src, addr: resolver}
}

// Address reports which resolver this will ask.
func (r *Resolver) Address() common.Address { return r.addr }

// Lookup resolves a chain name to its id and display metadata.
//
// The label may be given bare ("base") or whole ("base.on.eth"); anything under a
// different parent is refused rather than resolved, because this package speaks for
// the chain registry and not for ENS in general.
func (r *Resolver) Lookup(ctx context.Context, label string) (*Chain, error) {
	name, err := Qualify(label)
	if err != nil {
		return nil, err
	}

	id, err := r.ChainID(ctx, name)
	if err != nil {
		return nil, err
	}

	c := &Chain{
		Label:    strings.TrimSuffix(name, "."+Parent),
		Name:     name,
		ChainID:  id,
		Resolver: r.addr,
	}
	// Decoration, so a failure here must not lose the chain id we came for.
	c.URL = r.text(ctx, name, "url")
	c.Avatar = r.text(ctx, name, "avatar")
	return c, nil
}

// ChainID reads just the interoperable-address record and decodes the chain id.
// name must already be qualified.
func (r *Resolver) ChainID(ctx context.Context, name string) (uint64, error) {
	raw, err := r.record(ctx, name, selData, KeyInteropAddress)
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, fmt.Errorf("%w: %s", ErrNotRegistered, name)
	}
	return DecodeChainID(raw)
}

// text reads one ENSIP-5 record, best-effort.
func (r *Resolver) text(ctx context.Context, name, key string) string {
	raw, err := r.record(ctx, name, selText, key)
	if err != nil || len(raw) == 0 {
		return ""
	}
	// A registry entry is somebody else's string. Clean it for the same reason
	// token metadata is cleaned: it ends up in JSON and in a terminal.
	if len(raw) > maxRecord {
		raw = raw[:maxRecord]
	}
	return token.Clean(string(raw))
}

// record fetches one record's value, which takes two unwraps rather than one.
//
// resolve() returns the inner call's *return data*, not the record: the eth_call
// reply is abi.encode(bytes) around what data() or text() themselves encoded, and
// those are abi.encode(bytes) / abi.encode(string) in turn. Decoding one level and
// stopping yields a value whose first word is an ABI offset, which then parses as
// an ERC-7930 record of version zero — plausible-looking nonsense. Both records
// happen to be dynamic types, so one helper covers them.
func (r *Resolver) record(ctx context.Context, name string, sel []byte, key string) ([]byte, error) {
	out, err := r.resolve(ctx, name, encodeNodeString(sel, Namehash(name), key))
	if err != nil {
		return nil, err
	}
	ret, err := decodeBytes(out)
	if err != nil {
		return nil, err
	}
	// A name with no record at all resolves to zero-length return data, which is
	// the registry's way of saying no rather than an error to report.
	if len(ret) == 0 {
		return nil, nil
	}
	return decodeBytes(ret)
}

// maxRecord caps a text record. A registry entry is somebody else's data.
const maxRecord = 2048

// resolve performs the ENSIP-10 wildcard call. The chain resolver is a wildcard
// resolver — base.on.eth has no resolver of its own in the ENS registry — so the
// record call is nested inside resolve(dnsEncode(name), innerCalldata).
func (r *Resolver) resolve(ctx context.Context, name string, inner []byte) ([]byte, error) {
	data := append([]byte{}, selResolve...)
	// Two dynamic bytes arguments: offsets, then each length-prefixed and padded.
	data = append(data, common.LeftPadBytes([]byte{0x40}, 32)...)
	dns := DNSEncode(name)
	offset2 := 64 + 32 + pad32(len(dns))
	data = append(data, common.LeftPadBytes(uint64Bytes(uint64(offset2)), 32)...)
	data = append(data, encodeBytes(dns)...)
	data = append(data, encodeBytes(inner)...)

	to := r.addr
	return r.src.CallAtHead(ctx, ethereum.CallMsg{To: &to, Data: data})
}

// Qualify turns "base" or "base.on.eth" into "base.on.eth".
func Qualify(label string) (string, error) {
	l := strings.ToLower(strings.TrimSpace(strings.Trim(label, ".")))
	if l == "" {
		return "", errors.New("ens: empty chain name")
	}
	if !strings.Contains(l, ".") {
		return l + "." + Parent, nil
	}
	if !strings.HasSuffix(l, "."+Parent) {
		return "", fmt.Errorf("ens: %q is not a name under %s", label, Parent)
	}
	if strings.Count(l, ".") != strings.Count(Parent, ".")+1 {
		return "", fmt.Errorf("ens: %q is not a direct label of %s", label, Parent)
	}
	return l, nil
}

// Namehash is ENSIP-1.
func Namehash(name string) common.Hash {
	var node common.Hash
	if name == "" {
		return node
	}
	labels := strings.Split(name, ".")
	for i := len(labels) - 1; i >= 0; i-- {
		h := crypto.Keccak256Hash([]byte(labels[i]))
		node = crypto.Keccak256Hash(node.Bytes(), h.Bytes())
	}
	return node
}

// DNSEncode is the ENSIP-10 wire name: each label length-prefixed, terminated by a
// zero byte.
func DNSEncode(name string) []byte {
	out := make([]byte, 0, len(name)+2)
	for _, l := range strings.Split(name, ".") {
		if l == "" {
			continue
		}
		out = append(out, byte(len(l)))
		out = append(out, l...)
	}
	return append(out, 0)
}

// DNSDecode is the inverse of DNSEncode: the dotted name a wire name spells. Strict,
// because the gateway hashes the request as received and parses this from it — a
// name that half-decodes must be refused, not guessed at.
func DNSDecode(b []byte) (string, error) {
	var labels []string
	i := 0
	for {
		if i >= len(b) {
			return "", errors.New("ens: wire name has no terminator")
		}
		n := int(b[i])
		i++
		if n == 0 {
			break
		}
		if n > 63 {
			return "", fmt.Errorf("ens: wire label of %d bytes exceeds 63", n)
		}
		if i+n > len(b) {
			return "", errors.New("ens: wire name truncated inside a label")
		}
		labels = append(labels, string(b[i:i+n]))
		i += n
	}
	if i != len(b) {
		return "", fmt.Errorf("ens: %d trailing bytes after the wire name", len(b)-i)
	}
	return strings.Join(labels, "."), nil
}

// DecodeChainID reads an ERC-7930 v1 interoperable address that names a chain and
// no account.
//
//	0001 | 0000 | <refLen> | <chain id, big endian> | 00
//	 ^ver   ^EVM              ^reference               ^address length
//
// Strict on all three of version, chain type and a zero-length address. A record
// that is not a chain-only EVM address is not something to interpret generously:
// guessing wrong here means an index built under the wrong chain id, which is the
// exact failure this package exists to prevent.
func DecodeChainID(b []byte) (uint64, error) {
	if len(b) < 6 {
		return 0, fmt.Errorf("ens: interoperable address is %d bytes, too short", len(b))
	}
	if b[0] != 0x00 || b[1] != 0x01 {
		return 0, fmt.Errorf("ens: interoperable address version 0x%02x%02x is not 1", b[0], b[1])
	}
	if b[2] != 0x00 || b[3] != 0x00 {
		return 0, fmt.Errorf("ens: chain type 0x%02x%02x is not EVM", b[2], b[3])
	}
	refLen := int(b[4])
	if refLen == 0 || refLen > 8 {
		return 0, fmt.Errorf("ens: chain reference length %d is not a uint64", refLen)
	}
	if len(b) < 5+refLen+1 {
		return 0, fmt.Errorf("ens: interoperable address truncated at the chain reference")
	}
	var id uint64
	for _, c := range b[5 : 5+refLen] {
		id = id<<8 | uint64(c)
	}
	if addrLen := b[5+refLen]; addrLen != 0 {
		return 0, fmt.Errorf("ens: record names an address (%d bytes), not a chain", addrLen)
	}
	if id == 0 {
		return 0, errors.New("ens: chain id 0 is not a chain")
	}
	return id, nil
}

// --------------------------------------------------------------------------
// ABI encoding, hand-written like the rest of the call sites in this repo
// (internal/token, internal/lens/wire.go). Two dynamic arguments is not a
// dependency's worth of work.
// --------------------------------------------------------------------------

func pad32(n int) int { return (n + 31) / 32 * 32 }

// encodeNodeString builds `sel(bytes32 node, string key)`, which is the shape both
// data() and text() take.
func encodeNodeString(sel []byte, node common.Hash, key string) []byte {
	out := append([]byte{}, sel...)
	out = append(out, node.Bytes()...)
	out = append(out, common.LeftPadBytes([]byte{0x40}, 32)...)
	return append(out, encodeBytes([]byte(key))...)
}

func uint64Bytes(v uint64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return b[:]
}

// encodeBytes is a length prefix and the body, right-padded to a word.
func encodeBytes(b []byte) []byte {
	out := append([]byte{}, common.LeftPadBytes(uint64Bytes(uint64(len(b))), 32)...)
	out = append(out, b...)
	if r := len(b) % 32; r != 0 {
		out = append(out, make([]byte, 32-r)...)
	}
	return out
}

// decodeBytes unwraps a single `bytes` return value.
func decodeBytes(out []byte) ([]byte, error) {
	if len(out) == 0 {
		return nil, nil
	}
	if len(out) < 64 {
		return nil, fmt.Errorf("ens: short return (%d bytes)", len(out))
	}
	off := new(uint256).setBytes(out[:32])
	if !off.ok() || off.v+32 > uint64(len(out)) {
		return nil, errors.New("ens: bytes offset out of range")
	}
	n := new(uint256).setBytes(out[off.v : off.v+32])
	if !n.ok() {
		return nil, errors.New("ens: bad bytes length")
	}
	start := off.v + 32
	if start+n.v > uint64(len(out)) {
		return nil, errors.New("ens: bytes body out of range")
	}
	return out[start : start+n.v], nil
}

// uint256 is a 32-byte word that knows whether it fits in a uint64. A hostile or
// merely broken record can claim any offset it likes.
type uint256 struct {
	v  uint64
	hi bool
}

func (u *uint256) setBytes(b []byte) *uint256 {
	for _, c := range b[:len(b)-8] {
		if c != 0 {
			u.hi = true
		}
	}
	for _, c := range b[len(b)-8:] {
		u.v = u.v<<8 | uint64(c)
	}
	return u
}

func (u *uint256) ok() bool { return !u.hi }
