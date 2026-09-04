// Package token probes contracts over eth_call.
//
// Every call here targets head state, never a historical block, because the design
// deliberately requires only a snap-synced node. Balances are read live rather than
// derived from summed Transfer events: rebasing, fee-on-transfer and upgradeable
// tokens all make event-derived balances wrong, and the node can just tell us.
package token

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/chain"
)

// Function selectors.
var (
	selSymbol      = selector("symbol()")
	selName        = selector("name()")
	selDecimals    = selector("decimals()")
	selBalanceOf   = selector("balanceOf(address)")
	sel1155Balance = selector("balanceOf(address,uint256)")
)

func selector(sig string) []byte {
	return crypto.Keccak256([]byte(sig))[:4]
}

// Metadata is what a contract says about itself. Every field is best-effort: plenty of
// real tokens implement none of these.
type Metadata struct {
	Symbol   string
	Name     string
	Decimals *int16
}

// Probe reads symbol, name and decimals from a contract.
func Probe(ctx context.Context, src chain.Source, addr common.Address) Metadata {
	var m Metadata
	if s, err := callString(ctx, src, addr, selSymbol); err == nil {
		m.Symbol = s
	}
	if s, err := callString(ctx, src, addr, selName); err == nil {
		m.Name = s
	}
	if d, err := callUint(ctx, src, addr, selDecimals); err == nil && d.IsUint64() && d.Uint64() <= 77 {
		v := int16(d.Uint64())
		m.Decimals = &v
	}
	return m
}

// BalanceOf reads an ERC-20 or ERC-721 balance at head.
func BalanceOf(ctx context.Context, src chain.Source, tokenAddr, account common.Address) (*big.Int, error) {
	data := append(append([]byte{}, selBalanceOf...), common.LeftPadBytes(account.Bytes(), 32)...)
	return callUint(ctx, src, tokenAddr, data)
}

// BalanceOf1155 reads an ERC-1155 balance for one token id at head.
func BalanceOf1155(ctx context.Context, src chain.Source, tokenAddr, account common.Address, id *big.Int) (*big.Int, error) {
	data := append([]byte{}, sel1155Balance...)
	data = append(data, common.LeftPadBytes(account.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(id.Bytes(), 32)...)
	return callUint(ctx, src, tokenAddr, data)
}

func call(ctx context.Context, src chain.Source, addr common.Address, data []byte) ([]byte, error) {
	return src.CallAtHead(ctx, ethereum.CallMsg{To: &addr, Data: data})
}

func callUint(ctx context.Context, src chain.Source, addr common.Address, data []byte) (*big.Int, error) {
	out, err := call(ctx, src, addr, data)
	if err != nil {
		return nil, err
	}
	if len(out) < 32 {
		return nil, errors.New("token: short return")
	}
	return new(big.Int).SetBytes(out[:32]), nil
}

// callString decodes both the ABI string return and the bytes32 form that predates it
// (MKR and other early tokens return bytes32 from symbol() and name()).
func callString(ctx context.Context, src chain.Source, addr common.Address, data []byte) (string, error) {
	out, err := call(ctx, src, addr, data)
	if err != nil {
		return "", err
	}
	switch {
	case len(out) == 32:
		return clean(strings.TrimRight(string(out), "\x00")), nil

	case len(out) >= 64:
		offset := new(big.Int).SetBytes(out[:32])
		if !offset.IsUint64() {
			return "", errors.New("token: bad string offset")
		}
		off := offset.Uint64()
		if off+32 > uint64(len(out)) {
			return "", errors.New("token: string offset out of range")
		}
		length := new(big.Int).SetBytes(out[off : off+32])
		if !length.IsUint64() {
			return "", errors.New("token: bad string length")
		}
		n := length.Uint64()
		// Cap the copy: a hostile contract can claim any length it likes.
		if n > 1024 {
			n = 1024
		}
		start := off + 32
		if start+n > uint64(len(out)) {
			return "", errors.New("token: string body out of range")
		}
		return clean(string(out[start : start+n])), nil

	default:
		return "", errors.New("token: unrecognised string encoding")
	}
}

// clean drops control characters and invalid UTF-8 so a hostile token name cannot
// inject terminal escapes or break JSON consumers downstream.
func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s))
}
