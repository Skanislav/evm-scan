package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// fakeRegistry answers HintRegistry's immutable getters the way a deployed contract
// would, and counts how often each is asked, which is what the cache tests are about.
type fakeRegistry struct {
	abi   abi.ABI
	mode  hintreg.Mode
	calls map[string]int
	// failOnce makes the named getter fail on its first call only.
	failOnce map[string]bool
}

func newFakeRegistry(t *testing.T, mode hintreg.Mode) *fakeRegistry {
	t.Helper()
	parsed, err := contracts.HintRegistryABI()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeRegistry{abi: parsed, mode: mode, calls: map[string]int{}, failOnce: map[string]bool{}}
}

func (f *fakeRegistry) CallAtHead(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	m, err := f.abi.MethodById(msg.Data[:4])
	if err != nil {
		return nil, err
	}
	f.calls[m.Name]++
	if f.failOnce[m.Name] {
		delete(f.failOnce, m.Name)
		return nil, errors.New("node not ready")
	}
	var out any
	switch m.Name {
	case "oracle":
		out = f.mode.Oracle
	case "bondCurrency":
		out = f.mode.BondCurrency
	case "arbiter":
		out = f.mode.Arbiter
	case "assetBond":
		out = f.mode.AssetBond
	case "publisherBond":
		out = f.mode.PublisherBond
	case "challengeWindow":
		out = new(big.Int).SetUint64(f.mode.ChallengeWindow)
	case "rewardPerBlock":
		out = big.NewInt(100)
	case "minFunding":
		out = big.NewInt(1000)
	default:
		return nil, errors.New("unexpected call " + m.Name)
	}
	return m.Outputs.Pack(out)
}

var (
	localArbiterMode = hintreg.Mode{
		Arbiter:         common.HexToAddress("0x00000000000000000000000000000000000000a1"),
		PublisherBond:   big.NewInt(0),
		AssetBond:       big.NewInt(0),
		ChallengeWindow: 3600,
	}
	oracleMode = hintreg.Mode{
		Oracle:          common.HexToAddress("0x00000000000000000000000000000000000000c1"),
		BondCurrency:    common.HexToAddress("0x00000000000000000000000000000000000000c2"),
		PublisherBond:   big.NewInt(5_000_000),
		AssetBond:       big.NewInt(7),
		ChallengeWindow: 7200,
	}
)

func TestRegistryStatusFromNamesTheMode(t *testing.T) {
	addr := common.HexToAddress("0x00000000000000000000000000000000000000ee")

	local := registryStatusFrom(11155111, addr, &localArbiterMode)
	if local.Adjudication != "local-arbiter" || local.Arbiter != localArbiterMode.Arbiter.Hex() {
		t.Fatalf("local-arbiter block wrong: %+v", local)
	}
	if local.Oracle != "" || local.BondCurrency != "" {
		t.Fatalf("local-arbiter block must not carry oracle fields: %+v", local)
	}
	if local.PublisherBondWei != "0" || local.AssetBondWei != "0" || *local.ChallengeWindowSeconds != 3600 {
		t.Fatalf("economics wrong: %+v", local)
	}

	oracle := registryStatusFrom(1, addr, &oracleMode)
	if oracle.Adjudication != "optimistic-oracle" || oracle.Oracle != oracleMode.Oracle.Hex() ||
		oracle.BondCurrency != oracleMode.BondCurrency.Hex() {
		t.Fatalf("oracle block wrong: %+v", oracle)
	}
	if oracle.Arbiter != "" {
		t.Fatalf("oracle block must not name an arbiter: %+v", oracle)
	}
	if oracle.PublisherBondWei != "5000000" || oracle.AssetBondWei != "7" || *oracle.ChallengeWindowSeconds != 7200 {
		t.Fatalf("economics wrong: %+v", oracle)
	}

	none := registryStatusFrom(1, addr, nil)
	if none.Adjudication != "" || none.ChallengeWindowSeconds != nil || none.Address != addr.Hex() {
		t.Fatalf("nil mode must leave the mode fields out: %+v", none)
	}
}

// statusRegistry runs one GET /v1/status and returns the registry block, or nil when
// the response has none.
func statusRegistry(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Registry map[string]any `json:"registry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Registry
}

func TestStatusReadsTheModeOnceAndReusesTheBond(t *testing.T) {
	fake := newFakeRegistry(t, localArbiterMode)
	client, err := hintreg.NewClient(fake, common.HexToAddress("0xee"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Registry: client, RegistryChainID: 11155111, Log: slog.Default()})

	reg := statusRegistry(t, s)
	if reg["adjudication"] != "local-arbiter" || reg["publisher_bond_wei"] != "0" ||
		reg["challenge_window_seconds"] != float64(3600) || reg["asset_bond_wei"] != "0" {
		t.Fatalf("first status wrong: %v", reg)
	}
	if reg["reward_per_block_wei"] != "100" || reg["min_funding_wei"] != "1000" {
		t.Fatalf("live economics missing: %v", reg)
	}

	statusRegistry(t, s)
	statusRegistry(t, s)
	if fake.calls["oracle"] != 1 || fake.calls["challengeWindow"] != 1 {
		t.Fatalf("mode must be read once, got %v", fake.calls)
	}
	if fake.calls["assetBond"] != 1 {
		t.Fatalf("asset bond must come from the cached mode, got %d reads", fake.calls["assetBond"])
	}
	// The two values Mode does not carry stay live.
	if fake.calls["rewardPerBlock"] != 3 || fake.calls["minFunding"] != 3 {
		t.Fatalf("reward and funding should be read per request, got %v", fake.calls)
	}
}

func TestStatusTrustsAModeReadAtStartup(t *testing.T) {
	fake := newFakeRegistry(t, oracleMode)
	client, err := hintreg.NewClient(fake, common.HexToAddress("0xee"))
	if err != nil {
		t.Fatal(err)
	}
	preset := oracleMode
	s := New(Deps{Registry: client, RegistryChainID: 1, RegistryMode: &preset, Log: slog.Default()})

	reg := statusRegistry(t, s)
	if reg["adjudication"] != "optimistic-oracle" || reg["oracle"] != oracleMode.Oracle.Hex() ||
		reg["bond_currency"] != oracleMode.BondCurrency.Hex() || reg["arbiter"] != nil {
		t.Fatalf("oracle status wrong: %v", reg)
	}
	if fake.calls["oracle"] != 0 || fake.calls["assetBond"] != 0 {
		t.Fatalf("a preset mode must not be re-read, got %v", fake.calls)
	}
}

func TestStatusRetriesAModeReadThatFailed(t *testing.T) {
	fake := newFakeRegistry(t, localArbiterMode)
	fake.failOnce["oracle"] = true
	client, err := hintreg.NewClient(fake, common.HexToAddress("0xee"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Registry: client, RegistryChainID: 11155111, Log: slog.Default()})

	reg := statusRegistry(t, s)
	if _, ok := reg["adjudication"]; ok {
		t.Fatalf("a failed mode read must not invent a mode: %v", reg)
	}
	// Without the mode the bond is read on its own, so the field is still there.
	if reg["asset_bond_wei"] != "0" {
		t.Fatalf("asset bond should fall back to a live read: %v", reg)
	}

	reg = statusRegistry(t, s)
	if reg["adjudication"] != "local-arbiter" {
		t.Fatalf("second read should succeed and be kept: %v", reg)
	}
	statusRegistry(t, s)
	if fake.calls["oracle"] != 2 {
		t.Fatalf("mode read once after the failure, got %d", fake.calls["oracle"])
	}
}

func TestStatusWithoutARegistryHasNoRegistryBlock(t *testing.T) {
	s := New(Deps{Log: slog.Default()})
	if reg := statusRegistry(t, s); reg != nil {
		t.Fatalf("no registry configured, yet the block is present: %v", reg)
	}
}
