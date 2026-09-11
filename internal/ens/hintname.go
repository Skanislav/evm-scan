package ens

// The names HintResolver serves, and the normalization every tool applies before
// hashing one.
//
// Nothing here resolves anything. Account names are resolved in the client — the
// page against the reader's own RPC, the mirror through its injected eth_call —
// and only ever become an address before the daemon sees them. What the daemon
// does know is the *shape* of the names it publishes the index under
// (`<hex>.hints.<parent>`), which is what HintName and ParseHintName encode.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/text/unicode/norm"
)

var (
	// ErrEmptyName is returned for "" and for names that are only dots.
	ErrEmptyName = errors.New("ens: empty name")
	// ErrBadLabel is wrapped around the offending label.
	ErrBadLabel = errors.New("ens: bad label")
)

// Normalize lowercases and NFC-normalizes a name, trims one trailing dot, and
// rejects empty labels, labels over 63 bytes (the DNS wire limit), whitespace and
// control characters. It is not ENSIP-15: a name with emoji sequences or
// confusable scripts may hash differently from the ENS app's rendering. The page
// and the mirror apply the identical transform, so the three agree with each other
// even where they disagree with the app.
func Normalize(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return "", ErrEmptyName
	}
	if len(name) > 512 {
		return "", fmt.Errorf("%w: name longer than 512 bytes", ErrBadLabel)
	}
	name = norm.NFC.String(name)
	labels := strings.Split(name, ".")
	for i, l := range labels {
		if l == "" {
			return "", fmt.Errorf("%w: empty label in %q", ErrBadLabel, name)
		}
		if len(l) > 63 {
			return "", fmt.Errorf("%w: %q is longer than 63 bytes", ErrBadLabel, l)
		}
		for _, r := range l {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return "", fmt.Errorf("%w: %q contains whitespace or a control character", ErrBadLabel, l)
			}
		}
		labels[i] = strings.ToLower(l)
	}
	return strings.Join(labels, "."), nil
}

// Labels splits a normalized name. "" yields no labels.
func Labels(name string) []string {
	if name == "" {
		return nil
	}
	return strings.Split(name, ".")
}

// HintName is the name HintResolver serves for an account: the lowercase hex
// address (no 0x) under `hints.<parent>`. When chainID differs from the resolver's
// default, the caller passes explicitChain and the chain id becomes a second label.
func HintName(account common.Address, parent string, chainID uint64, explicitChain bool) string {
	label := hex.EncodeToString(account[:])
	if explicitChain {
		return fmt.Sprintf("%s.%d.hints.%s", label, chainID, parent)
	}
	return label + ".hints." + parent
}

// ParseHintName is the inverse of HintName: the account and optional chain id in a
// name served by HintResolver. ok is false when the first label is not an address.
func ParseHintName(name string) (account common.Address, chainID uint64, hasChain bool, ok bool) {
	norm, err := Normalize(name)
	if err != nil {
		return common.Address{}, 0, false, false
	}
	labels := Labels(norm)
	if len(labels) == 0 {
		return common.Address{}, 0, false, false
	}
	first := strings.TrimPrefix(labels[0], "0x")
	if len(first) != 40 || !common.IsHexAddress("0x"+first) {
		return common.Address{}, 0, false, false
	}
	account = common.HexToAddress("0x" + first)
	if len(labels) > 1 {
		if id, err := parseDecimal(labels[1]); err == nil {
			return account, id, true, true
		}
	}
	return account, 0, false, true
}

func parseDecimal(s string) (uint64, error) {
	if s == "" || len(s) > 20 {
		return 0, errors.New("not decimal")
	}
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not decimal")
		}
		v = v*10 + uint64(c-'0')
	}
	return v, nil
}

// SplitContracts parses HintResolver's evmscan.contracts record.
func SplitContracts(text string) ([]common.Address, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	var out []common.Address
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if !common.IsHexAddress(part) {
			return nil, fmt.Errorf("ens: %q in evmscan.contracts is not an address", part)
		}
		out = append(out, common.HexToAddress(part))
	}
	return out, nil
}
