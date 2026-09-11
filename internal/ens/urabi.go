package ens

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/ccip"
)

// Resolver profile calls and Universal Resolver errors, for the tools that read
// HintResolver back (cmd/evmscan-verify, cmd/evmscan-ens check) and for the
// simulated-backend test that drives the resolver end to end. Every selector is
// derived from its signature; pasting hex would hide a typo until a live call
// failed.

// sel4 is selector as a fixed-size array, for switch statements over revert data.
func sel4(sig string) [4]byte { return [4]byte(selector(sig)) }

var (
	// Resolver profiles (ENSIP-1, ENSIP-5). selText is shared with the chain
	// registry reader in ens.go.
	selAddr = selector("addr(bytes32)")

	// Universal Resolver errors.
	errResolverNotFound    = sel4("ResolverNotFound(bytes)")
	errResolverNotContract = sel4("ResolverNotContract(bytes,address)")
	errUnsupportedProfile  = sel4("UnsupportedResolverProfile(bytes4)")
	errResolverError       = sel4("ResolverError(bytes)")
	errReverseMismatch     = sel4("ReverseAddressMismatch(string,bytes)")
	errHTTPError           = sel4("HttpError(uint16,string)")

	bytes32In   = mustArgs("bytes32")
	textIn      = mustArgs("bytes32", "string")
	stringOut   = mustArgs("string")
	bytesOut    = mustArgs("bytes")
	bytes4Out   = mustArgs("bytes4")
	nameErrArgs = mustArgs("bytes")
	strBytesErr = mustArgs("string", "bytes")
)

func mustArgs(types ...string) abi.Arguments {
	out := make(abi.Arguments, len(types))
	for i, t := range types {
		ty, err := abi.NewType(t, "", nil)
		if err != nil {
			panic(err)
		}
		out[i] = abi.Argument{Type: ty}
	}
	return out
}

func pack(sel []byte, args abi.Arguments, vals ...any) ([]byte, error) {
	body, err := args.Pack(vals...)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, sel...), body...), nil
}

// AddrCallData is the addr(node) profile call a resolver answers.
func AddrCallData(node common.Hash) ([]byte, error) {
	return pack(selAddr, bytes32In, node)
}

// TextCallData is the text(node, key) profile call a resolver answers.
func TextCallData(node common.Hash, key string) ([]byte, error) {
	return pack(selText, textIn, node, key)
}

// DecodeString reads an ABI-encoded string, as text() returns one.
func DecodeString(out []byte) (string, error) {
	vals, err := stringOut.Unpack(out)
	if err != nil {
		return "", err
	}
	s, _ := vals[0].(string)
	return s, nil
}

// DecodeBytes reads an ABI-encoded bytes value, as an extended resolver returns one.
func DecodeBytes(out []byte) ([]byte, error) {
	vals, err := bytesOut.Unpack(out)
	if err != nil {
		return nil, err
	}
	b, _ := vals[0].([]byte)
	return b, nil
}

// Selector is the 4-byte function selector of a signature, for callers that build a
// profile call this package has no helper for.
func Selector(sig string) []byte { return selector(sig) }

// ErrNotFound: no resolver for the name, or the record is empty.
var ErrNotFound = errors.New("ens: name does not resolve")

// ErrOffchain is returned when the name lives behind an ERC-3668 gateway that the
// caller chose not to follow.
type ErrOffchain struct {
	Lookup *ccip.Lookup
}

func (e *ErrOffchain) Error() string {
	return fmt.Sprintf("ens: name resolves offchain (CCIP-Read) via %v", e.Lookup.URLs)
}

// ErrResolverError wraps the resolver's own revert, as the Universal Resolver
// reports it.
type ErrResolverError struct {
	Data []byte
}

func (e *ErrResolverError) Error() string {
	return "ens: resolver reverted " + ccip.Selector(e.Data)
}

// ErrUnsupportedProfile: the resolver has no answer for this record type.
type ErrUnsupportedProfile struct {
	Selector [4]byte
}

func (e *ErrUnsupportedProfile) Error() string {
	return fmt.Sprintf("ens: resolver does not support profile 0x%x", e.Selector)
}

// ErrReverseMismatch: the primary name does not point back at the address.
type ErrReverseMismatch struct {
	Primary string
}

func (e *ErrReverseMismatch) Error() string {
	return fmt.Sprintf("ens: primary name %q does not resolve back to the address", e.Primary)
}

// classifyRevert maps Universal Resolver revert data to a typed error. Unknown
// reverts come back as a generic error carrying the selector.
func classifyRevert(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("ens: call reverted without data")
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	body := data[4:]
	switch sel {
	case ccip.OffchainLookupSelector:
		l, ok, err := ccip.ParseOffchainLookup(data)
		if err != nil || !ok {
			return fmt.Errorf("ens: malformed OffchainLookup: %w", err)
		}
		return &ErrOffchain{Lookup: l}
	case errResolverNotFound, errResolverNotContract:
		return ErrNotFound
	case errUnsupportedProfile:
		var e ErrUnsupportedProfile
		if vals, err := bytes4Out.Unpack(body); err == nil {
			e.Selector, _ = vals[0].([4]byte)
		}
		return &e
	case errResolverError:
		var inner []byte
		if vals, err := nameErrArgs.Unpack(body); err == nil {
			inner, _ = vals[0].([]byte)
		}
		return &ErrResolverError{Data: inner}
	case errReverseMismatch:
		var e ErrReverseMismatch
		if vals, err := strBytesErr.Unpack(body); err == nil {
			e.Primary, _ = vals[0].(string)
		}
		return &e
	case errHTTPError:
		return fmt.Errorf("ens: universal resolver reported a gateway HTTP error")
	}
	return fmt.Errorf("ens: call reverted with %s", ccip.Selector(data))
}
