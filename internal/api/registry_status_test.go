package api

import (
	"context"
	"encoding/json"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// fakeRegistry answers the registry's immutable getters and the bond currency's
// metadata by selector, and counts how often it is asked.
type fakeRegistry struct {
	registry, currency common.Address
	words              map[string][]byte // selector hex -> return data, for calls to the registry
	currencyWords      map[string][]byte // same, for calls to the currency
	calls              atomic.Int64
}

func (f *fakeRegistry) CallAtHead(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	f.calls.Add(1)
	table := f.words
	if msg.To != nil && *msg.To == f.currency {
		table = f.currencyWords
	}
	if out, ok := table[common.Bytes2Hex(msg.Data[:4])]; ok {
		return out, nil
	}
	return nil, ethereum.NotFound
}

func sel(sig string) string { return common.Bytes2Hex(crypto.Keccak256([]byte(sig))[:4]) }
func wordAddr(a common.Address) []byte {
	return common.LeftPadBytes(a.Bytes(), 32)
}
func wordUint(v int64) []byte { return common.LeftPadBytes(big.NewInt(v).Bytes(), 32) }
func abiString(s string) []byte {
	out := append(wordUint(32), wordUint(int64(len(s)))...)
	return append(out, common.RightPadBytes([]byte(s), 32)...)
}

func newRegistryServer(t *testing.T, f *fakeRegistry) *Server {
	t.Helper()
	f.registry = common.HexToAddress("0x9999999999999999999999999999999999999999")
	c, err := hintreg.NewClient(f, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{d: Deps{Registry: c, RegistryChainID: 11155111, ENSParent: "evm-scan.eth"}}
}

func economics(oracle, currency, arbiter common.Address) map[string][]byte {
	return map[string][]byte{
		sel("oracle()"):          wordAddr(oracle),
		sel("bondCurrency()"):    wordAddr(currency),
		sel("arbiter()"):         wordAddr(arbiter),
		sel("assetBond()"):       wordUint(1e16),
		sel("publisherBond()"):   wordUint(500000),
		sel("challengeWindow()"): wordUint(7200),
		sel("rewardPerBlock()"):  wordUint(100e9),
		sel("minFunding()"):      wordUint(5e16),
	}
}

func TestRegistryStatusOracleMode(t *testing.T) {
	oracle := common.HexToAddress("0x1111111111111111111111111111111111111111")
	usdc := common.HexToAddress("0x2222222222222222222222222222222222222222")
	f := &fakeRegistry{
		currency: usdc,
		words:    economics(oracle, usdc, common.Address{}),
		currencyWords: map[string][]byte{
			sel("symbol()"):   abiString("USDC"),
			sel("decimals()"): wordUint(6),
		},
	}
	s := newRegistryServer(t, f)
	got := s.registryStatus(context.Background())
	if got == nil {
		t.Fatal("no registry block")
	}
	if got.Mode != "oracle" || got.Oracle != oracle.Hex() || got.Arbiter != "" {
		t.Fatalf("mode: %+v", got)
	}
	if got.PublisherBond != "500000" || got.BondCurrency != usdc.Hex() || got.BondCurrencySymbol != "USDC" ||
		got.BondCurrencyDecimals == nil || *got.BondCurrencyDecimals != 6 {
		t.Fatalf("bond: %+v", got)
	}
	if got.ChallengeWindowSeconds != 7200 || got.RewardPerBlockWei != "100000000000" ||
		got.MinFundingWei != "50000000000000000" || got.AssetBondWei != "10000000000000000" {
		t.Fatalf("economics: %+v", got)
	}
	if got.ENSParent != "evm-scan.eth" || got.ChainID != 11155111 {
		t.Fatalf("identity: %+v", got)
	}

	// Immutable, so read once: a second status costs the chain nothing.
	n := f.calls.Load()
	s.registryStatus(context.Background())
	if f.calls.Load() != n {
		t.Fatalf("second status re-read the registry: %d -> %d calls", n, f.calls.Load())
	}

	// The wire names, which the page reads.
	raw, _ := json.Marshal(got)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"mode", "oracle", "publisher_bond", "bond_currency", "bond_currency_symbol",
		"bond_currency_decimals", "challenge_window_seconds", "reward_per_block_wei", "min_funding_wei", "asset_bond_wei"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %s", k, raw)
		}
	}
	if _, ok := m["arbiter"]; ok {
		t.Errorf("oracle mode must not name an arbiter: %s", raw)
	}
}

func TestRegistryStatusLocalArbiterMode(t *testing.T) {
	arbiter := common.HexToAddress("0x3333333333333333333333333333333333333333")
	f := &fakeRegistry{words: economics(common.Address{}, common.Address{}, arbiter)}
	s := newRegistryServer(t, f)
	got := s.registryStatus(context.Background())
	if got.Mode != "local-arbiter" || got.Arbiter != arbiter.Hex() {
		t.Fatalf("mode: %+v", got)
	}
	if got.Oracle != "" || got.BondCurrency != "" || got.BondCurrencySymbol != "" || got.BondCurrencyDecimals != nil {
		t.Fatalf("local mode leaked oracle fields: %+v", got)
	}
	if got.PublisherBond != "500000" {
		t.Fatalf("bond: %+v", got)
	}
}

// A registry that will not answer yet still identifies itself, and is asked again
// next time rather than remembered as unreadable.
func TestRegistryStatusUnreadableIsRetried(t *testing.T) {
	f := &fakeRegistry{words: map[string][]byte{}}
	s := newRegistryServer(t, f)
	got := s.registryStatus(context.Background())
	if got == nil || got.Address == "" || got.Mode != "" || got.PublisherBond != "" {
		t.Fatalf("partial answer: %+v", got)
	}
	f.words = economics(common.Address{}, common.Address{}, common.HexToAddress("0x4"))
	if got = s.registryStatus(context.Background()); got.Mode != "local-arbiter" {
		t.Fatalf("not retried: %+v", got)
	}
}
