package ens

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/internal/chain"
)

// The vectors below are not invented. Every one was captured from a live call to
// the chain resolver on Ethereum mainnet, so a change that breaks the encoding
// breaks these tests rather than being discovered against a real node later.

// calldata that mainnet accepted for resolve(dnsEncode("base.on.eth"),
// data(namehash("base.on.eth"), "interoperable-address")).
const goldenResolveCalldata = "0x9061b92300000000000000000000000000000000000000000000000000000000000000400000000000000000000000000000000000000000000000000000000000000080000000000000000000000000000000000000000000000000000000000000000d0462617365026f6e0365746800000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000084ecbfada340d581c1d53524df6850c4e63f8967a97f00b3c1deac30319fbb166a276d60f700000000000000000000000000000000000000000000000000000000000000400000000000000000000000000000000000000000000000000000000000000015696e7465726f70657261626c652d61646472657373000000000000000000000000000000000000000000000000000000000000000000000000000000"

// What the resolver actually returned for each name.
const (
	// The literal eth_call reply for base.on.eth. Two layers of abi.encode(bytes)
	// — resolve()'s own return, then data()'s — around 0001000002210500, which is
	// ERC-7930 v1, EVM, 2-byte reference 0x2105 = 8453, zero-length address.
	replyBase = "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000008" +
		"0001000002210500000000000000000000000000000000000000000000000000"
	// arc.on.eth: registered nowhere, so zero-length bytes and no revert.
	replyEmpty = "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	// text(node,"url") on base.on.eth -> "https://www.base.org/"
	replyURL = "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000060" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000015" +
		"68747470733a2f2f7777772e626173652e6f72672f000000000000000000000000"
)

func hexb(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("bad test vector: %v", err)
	}
	return b
}

// TestNamehash pins ENSIP-1 against values cast agrees with.
func TestNamehash(t *testing.T) {
	for name, want := range map[string]string{
		"":            "0x0000000000000000000000000000000000000000000000000000000000000000",
		"eth":         "0x93cdeb708b7545dc668eb9280176169d1c33cfd8ed6f04690a0bcc88a93fc4ae",
		"on.eth":      "0xcabf8262fe531c2a7e8cd86e06342bc27fc0591ecd562fbac88280abc18ef899",
		"base.on.eth": "0x40d581c1d53524df6850c4e63f8967a97f00b3c1deac30319fbb166a276d60f7",
	} {
		if got := Namehash(name); got.Hex() != want {
			t.Errorf("Namehash(%q) = %s, want %s", name, got.Hex(), want)
		}
	}
}

func TestDNSEncode(t *testing.T) {
	got := "0x" + hex.EncodeToString(DNSEncode("base.on.eth"))
	if want := "0x0462617365026f6e0365746800"; got != want {
		t.Errorf("DNSEncode = %s, want %s", got, want)
	}
}

// TestResolveCalldata is the one that matters most: the bytes we build must be the
// bytes mainnet answered, or the whole package is decorative.
func TestResolveCalldata(t *testing.T) {
	var got []byte
	n := &fakeNode{call: func(msg ethereum.CallMsg) ([]byte, error) {
		got = msg.Data
		return hexb(t, replyBase), nil
	}}

	if _, err := New(n, common.Address{}).ChainID(context.Background(), "base.on.eth"); err != nil {
		t.Fatalf("ChainID: %v", err)
	}
	want := hexb(t, goldenResolveCalldata)
	if !bytesEqual(got, want) {
		t.Errorf("calldata mismatch\n got %x\nwant %x", got, want)
	}
}

func TestLookup(t *testing.T) {
	n := &fakeNode{call: func(msg ethereum.CallMsg) ([]byte, error) {
		// The inner selector says which record is being asked for.
		switch {
		case containsSel(msg.Data, selData):
			return hexb(t, replyBase), nil
		case containsSel(msg.Data, selText):
			return hexb(t, replyURL), nil
		}
		return nil, errors.New("unexpected record")
	}}

	c, err := New(n, common.Address{}).Lookup(context.Background(), "base")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if c.ChainID != 8453 {
		t.Errorf("chain id = %d, want 8453", c.ChainID)
	}
	if c.Name != "base.on.eth" || c.Label != "base" {
		t.Errorf("name = %q label = %q", c.Name, c.Label)
	}
	if c.URL != "https://www.base.org/" {
		t.Errorf("url = %q", c.URL)
	}
	if c.Resolver != ChainResolver {
		t.Errorf("resolver = %s, want the well-known one", c.Resolver)
	}
}

// TestUnregistered pins the behaviour the fallback path depends on: a name nobody
// has registered is an ordinary answer, not an error to report as a fault.
func TestUnregistered(t *testing.T) {
	n := &fakeNode{call: func(ethereum.CallMsg) ([]byte, error) {
		return hexb(t, replyEmpty), nil
	}}
	_, err := New(n, common.Address{}).Lookup(context.Background(), "arc")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("err = %v, want ErrNotRegistered", err)
	}
}

// TestDecorationIsOptional: a text record that fails must not cost us the chain id,
// which is the only field anyone actually needs.
func TestDecorationIsOptional(t *testing.T) {
	n := &fakeNode{call: func(msg ethereum.CallMsg) ([]byte, error) {
		if containsSel(msg.Data, selText) {
			return nil, errors.New("resolver does not do text")
		}
		return hexb(t, replyBase), nil
	}}
	c, err := New(n, common.Address{}).Lookup(context.Background(), "base")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if c.ChainID != 8453 || c.URL != "" {
		t.Errorf("got chain %d url %q", c.ChainID, c.URL)
	}
}

func TestDecodeChainID(t *testing.T) {
	ok := map[string]uint64{
		"00010000010100":   1,     // ethereum.on.eth
		"00010000010a00":   10,    // optimism.on.eth
		"0001000002210500": 8453,  // base.on.eth
		"0001000002a4b100": 42161, // arbitrum.on.eth
	}
	for in, want := range ok {
		b, _ := hex.DecodeString(strings.ReplaceAll(in, " ", ""))
		got, err := DecodeChainID(b)
		if err != nil {
			t.Errorf("DecodeChainID(%s): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("DecodeChainID(%s) = %d, want %d", in, got, want)
		}
	}

	// Refusals. Guessing here would mean indexing under the wrong chain id, so
	// each of these must be an error rather than a best effort.
	bad := map[string]string{
		"0002000002210500": "a version we do not know",
		"0001000102210500": "a chain type that is not EVM",
		"000100000221051400000000000000000000000000000000000000000000": "a record naming an account, not a chain",
		"000100000000": "a zero-length chain reference",
		"000100000100": "chain id zero",
		"00010000":     "truncated before the reference",
	}
	for in, why := range bad {
		b, _ := hex.DecodeString(in)
		if _, err := DecodeChainID(b); err == nil {
			t.Errorf("DecodeChainID(%s) accepted %s", in, why)
		}
	}
}

func TestQualify(t *testing.T) {
	for in, want := range map[string]string{
		"base":        "base.on.eth",
		"  BASE  ":    "base.on.eth",
		"base.on.eth": "base.on.eth",
		"Base.On.Eth": "base.on.eth",
	} {
		got, err := Qualify(in)
		if err != nil || got != want {
			t.Errorf("Qualify(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A name under some other parent is not ours to resolve, and neither is a
	// deeper label: this package speaks for the chain registry only.
	for _, in := range []string{"", "  ", "vitalik.eth", "a.b.on.eth", "base.off.eth"} {
		if got, err := Qualify(in); err == nil {
			t.Errorf("Qualify(%q) = %q, want an error", in, got)
		}
	}
}

// --------------------------------------------------------------------------

func containsSel(data, sel []byte) bool {
	for i := 0; i+4 <= len(data); i++ {
		if bytesEqual(data[i:i+4], sel) {
			return true
		}
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeNode is a chain.Source that only answers eth_call, which is all this package
// ever asks for — and that is itself worth asserting.
type fakeNode struct {
	call func(ethereum.CallMsg) ([]byte, error)
}

func (f *fakeNode) CallAtHead(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	return f.call(msg)
}

func (f *fakeNode) ChainID(context.Context) (uint64, error)   { return 1, nil }
func (f *fakeNode) HeadBlock(context.Context) (uint64, error) { return 0, nil }
func (f *fakeNode) HeaderHash(context.Context, uint64) (common.Hash, error) {
	panic("ens must not read headers")
}
func (f *fakeNode) Logs(context.Context, chain.Query) ([]types.Log, error) {
	panic("ens must not call eth_getLogs")
}
func (f *fakeNode) SubscribeLogs(context.Context, chain.Query, chan<- types.Log) (ethereum.Subscription, error) {
	panic("ens must not subscribe")
}
func (f *fakeNode) CodeAt(context.Context, common.Address) ([]byte, error) {
	panic("ens must not read code")
}
func (f *fakeNode) NonceAt(context.Context, common.Address) (uint64, error) {
	panic("ens must not read nonces")
}
func (f *fakeNode) Endpoint() chain.Endpoint { return chain.Endpoint{} }
func (f *fakeNode) Close()                   {}

func TestDNSDecodeIsTheInverseOfEncode(t *testing.T) {
	for _, name := range []string{"base.on.eth", "abcdef0123456789abcdef0123456789abcdef01.hints.evm-scan.eth", "eth"} {
		got, err := DNSDecode(DNSEncode(name))
		if err != nil || got != name {
			t.Errorf("DNSDecode(DNSEncode(%q)) = %q, %v", name, got, err)
		}
	}
	for _, bad := range [][]byte{
		{},                                      // no terminator
		{3, 'a', 'b'},                           // truncated inside a label
		{1, 'a', 0, 0},                          // trailing byte
		append([]byte{64}, make([]byte, 64)...), // label over 63
	} {
		if _, err := DNSDecode(bad); err == nil {
			t.Errorf("DNSDecode(%x) accepted a malformed name", bad)
		}
	}
}

func TestEncodeStringRoundTrip(t *testing.T) {
	enc, err := EncodeString("0xabc,0xdef")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeString(enc); err != nil || got != "0xabc,0xdef" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}
