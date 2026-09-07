package hintreg_test

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// The dispute path is the part of HintRegistry that moves money, so it is exercised
// against a real EVM rather than reasoned about. Skipped unless EVMSCAN_TEST_NODE points
// at a dev node with an unlocked, funded account:
//
//	geth --dev --dev.period 1 --datadir /tmp/devchain --ipcpath /tmp/devchain/geth.ipc
//	EVMSCAN_TEST_NODE=/tmp/devchain/geth.ipc go test ./internal/hintreg/ -run Registry -v
//
// The oracle here is MockOptimisticOracleV3: no chain small enough to test on has a UMA
// deployment, and what these tests check is the registry's half of the protocol — that
// it asserts, routes disputes, and reacts to the oracle's callbacks correctly.

const liveness = 3 // seconds; short enough to wait out in a test

func TestRegistryOracleModeUndisputed(t *testing.T) {
	h := newHarness(t)
	env := h.deployOracleMode(t)
	publisher := h.fundedKey(t, "publisher")
	publisherAddr := h.fundBond(t, env, publisher)

	before := h.balanceOf(t, env.bondToken, publisherAddr)
	epochID := h.publish(t, env, publisher, 1, 100)

	e := h.epoch(t, env.registry, epochID)
	if e.Status != hintreg.EpochProposed {
		t.Fatalf("status = %s, want proposed", e.Status)
	}
	if e.AssertionID == (common.Hash{}) {
		t.Fatal("publishing in oracle mode left no assertion id")
	}
	if got := h.balanceOf(t, env.bondToken, publisherAddr); got.Cmp(new(big.Int).Sub(before, env.bond)) != 0 {
		t.Fatalf("publisher balance = %s, want %s (bond escrowed)", got, new(big.Int).Sub(before, env.bond))
	}

	// Settling before liveness expires must fail, or the challenge window means nothing.
	if err := h.simulate(t, env.registry, "finalizeIndex", big.NewInt(epochID)); err == nil {
		t.Fatal("finalizeIndex succeeded while the assertion was still live")
	}

	h.waitLiveness()
	h.sendFaucet(t, &env.registry, nil, h.pack(t, h.registryABI(t), "finalizeIndex", big.NewInt(epochID)))

	e = h.epoch(t, env.registry, epochID)
	if e.Status != hintreg.EpochFinalized {
		t.Fatalf("status = %s, want finalized", e.Status)
	}
	if got := h.balanceOf(t, env.bondToken, publisherAddr); got.Cmp(before) != 0 {
		t.Fatalf("publisher balance = %s, want its bond back (%s)", got, before)
	}

	// latestFinalizedEpoch is what a consumer reads, so it has to have moved.
	vals := h.call(t, env.registry, h.registryABI(t), "latestFinalizedEpoch", uint64(1))
	if found, _ := vals[0].(bool); !found {
		t.Fatal("latestFinalizedEpoch reports nothing after finalization")
	}
}

func TestRegistryOracleModeDisputeUpheldForChallenger(t *testing.T) {
	h := newHarness(t)
	env := h.deployOracleMode(t)
	publisher := h.fundedKey(t, "publisher")
	h.fundBond(t, env, publisher)
	challenger := h.fundedKey(t, "challenger")
	challengerAddr := h.fundBond(t, env, challenger)

	before := h.balanceOf(t, env.bondToken, challengerAddr)
	epochID := h.publish(t, env, publisher, 1, 100)

	// Dispute through the registry: it pulls the matching bond and forwards it.
	h.approve(t, challenger, env.bondToken, env.registry, env.bond)
	h.sendKey(t, challenger, &env.registry, nil, h.pack(t, h.registryABI(t), "challengeIndex", big.NewInt(epochID)))

	e := h.epoch(t, env.registry, epochID)
	if e.Status != hintreg.EpochChallenged {
		t.Fatalf("status = %s, want challenged", e.Status)
	}
	if e.Challenger != challengerAddr {
		t.Fatalf("challenger = %s, want %s", e.Challenger.Hex(), challengerAddr.Hex())
	}

	// Waiting out liveness must not finalize a disputed commitment: only the oracle's
	// verdict can settle it now.
	h.waitLiveness()
	if err := h.simulate(t, env.registry, "finalizeIndex", big.NewInt(epochID)); err == nil {
		t.Fatal("finalizeIndex succeeded before the oracle voted on the dispute")
	}

	// The mock's stand-in for a DVM vote: the commitment was wrong.
	oracleABI := h.abiOf(t, "MockOptimisticOracleV3")
	h.sendFaucet(t, &env.oracle, nil, h.pack(t, oracleABI, "resolveDispute", [32]byte(e.AssertionID), false))
	h.sendFaucet(t, &env.registry, nil, h.pack(t, h.registryABI(t), "finalizeIndex", big.NewInt(epochID)))

	e = h.epoch(t, env.registry, epochID)
	if e.Status != hintreg.EpochRejected {
		t.Fatalf("status = %s, want rejected", e.Status)
	}
	// The challenger was right, so it holds both bonds.
	want := new(big.Int).Add(before, env.bond)
	if got := h.balanceOf(t, env.bondToken, challengerAddr); got.Cmp(want) != 0 {
		t.Fatalf("challenger balance = %s, want %s", got, want)
	}
	// A rejected root must not become the answer consumers read.
	vals := h.call(t, env.registry, h.registryABI(t), "latestFinalizedEpoch", uint64(1))
	if found, _ := vals[0].(bool); found {
		t.Fatal("a rejected commitment was recorded as the latest finalized epoch")
	}
}

func TestRegistryOracleModeHasNoArbiter(t *testing.T) {
	h := newHarness(t)
	env := h.deployOracleMode(t)
	reg := h.registryABI(t)

	vals := h.call(t, env.registry, reg, "arbiter")
	if got, _ := vals[0].(common.Address); got != (common.Address{}) {
		t.Fatalf("arbiter = %s, want the zero address in oracle mode", got.Hex())
	}

	// Every admin entry point must be unreachable, including from the deployer.
	for _, c := range []struct {
		method string
		args   []any
	}{
		{"setArbiter", []any{h.faucet}},
		{"setBonds", []any{big.NewInt(1), big.NewInt(1), big.NewInt(1)}},
		{"resolveChallenge", []any{big.NewInt(0), true}},
		{"setEconomics", []any{big.NewInt(1), big.NewInt(1)}},
		{"setGateways", []any{[]string{"https://example.invalid/{sender}/{data}.json"}}},
	} {
		if err := h.simulate(t, env.registry, c.method, c.args...); err == nil {
			t.Fatalf("%s succeeded in oracle mode", c.method)
		}
	}
}

func TestRegistryLocalArbiterModeStillWorks(t *testing.T) {
	h := newHarness(t)
	reg := h.registryABI(t)
	bond := big.NewInt(1_000_000)

	// The fallback for chains with no oracle: the deployer settles disputes.
	registry := h.deploy(t, "HintRegistry", hintreg.ConstructorArgs(
		common.Address{}, common.Address{}, h.faucet, testEconomics(bond), nil)...)

	client := h.client(t, registry)
	mode, err := client.Mode(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mode.OracleMode() || mode.Arbiter != h.faucet {
		t.Fatalf("mode = %s, want local-arbiter with the deployer as arbiter", mode)
	}

	publisher := h.fundedKey(t, "publisher")
	h.sendKey(t, publisher, &registry, bond, h.pack(t, reg, "publishIndex",
		uint64(1), uint64(1), uint64(100), [32]byte(common.HexToHash("0xfeed")), [32]byte{}, "ipfs://local"))

	challenger := h.fundedKey(t, "challenger")
	challengerAddr := crypto.PubkeyToAddress(challenger.PublicKey)
	before := h.ethBalance(t, challengerAddr)
	h.sendKey(t, challenger, &registry, bond, h.pack(t, reg, "challengeIndex", big.NewInt(0)))

	// The arbiter rules for the challenger, who collects both bonds.
	h.sendFaucet(t, &registry, nil, h.pack(t, reg, "resolveChallenge", big.NewInt(0), false))

	e := h.epoch(t, registry, 0)
	if e.Status != hintreg.EpochRejected {
		t.Fatalf("status = %s, want rejected", e.Status)
	}
	if got := h.ethBalance(t, challengerAddr); got.Cmp(before) <= 0 {
		t.Fatalf("challenger balance %s did not grow from %s after winning", got, before)
	}
}

// --------------------------------------------------------------------------
// Harness
// --------------------------------------------------------------------------

// testEconomics prices a test registry: no asset bond, the given publisher bond, the
// short liveness, and no coverage rewards (those have their own tests).
func testEconomics(publisherBond *big.Int) hintreg.Economics {
	return hintreg.Economics{
		AssetBond:       big.NewInt(0),
		PublisherBond:   publisherBond,
		ChallengeWindow: big.NewInt(liveness),
		MinFunding:      big.NewInt(0),
		RewardPerBlock:  big.NewInt(0),
	}
}

type oracleEnv struct {
	registry  common.Address
	oracle    common.Address
	bondToken common.Address
	bond      *big.Int
}

type harness struct {
	ctx     context.Context
	node    *chain.Node
	eth     *ethclient.Client
	chainID *big.Int
	faucet  common.Address
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	endpoint := os.Getenv("EVMSCAN_TEST_NODE")
	if endpoint == "" {
		t.Skip("set EVMSCAN_TEST_NODE to a dev node with an unlocked account to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	node, err := chain.Dial(ctx, endpoint, false)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	t.Cleanup(node.Close)

	chainID, err := node.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := node.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Skipf("node has no unlocked account: %v", err)
	}

	eth, err := ethclient.DialContext(ctx, endpoint)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	t.Cleanup(eth.Close)

	return &harness{
		ctx:     ctx,
		node:    node,
		eth:     eth,
		chainID: new(big.Int).SetUint64(chainID),
		faucet:  accounts[0],
	}
}

// deployOracleMode brings up a bond token, a mock oracle and a registry wired to both.
func (h *harness) deployOracleMode(t *testing.T) oracleEnv {
	t.Helper()
	bond := big.NewInt(1_000_000)
	token := h.deploy(t, "DemoERC20", "Bond Token", "BOND")
	// The faucet stands in for UMA's DVM.
	oracle := h.deploy(t, "MockOptimisticOracleV3", h.faucet, big.NewInt(0))
	registry := h.deploy(t, "HintRegistry", hintreg.ConstructorArgs(
		oracle, token, common.Address{}, testEconomics(bond), nil)...)

	client := h.client(t, registry)
	mode, err := client.Mode(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !mode.OracleMode() || mode.Oracle != oracle || mode.BondCurrency != token {
		t.Fatalf("mode = %s, want oracle mode against %s", mode, oracle.Hex())
	}
	return oracleEnv{registry: registry, oracle: oracle, bondToken: token, bond: bond}
}

// publish posts a commitment as `key`, approving the bond first.
func (h *harness) publish(t *testing.T, env oracleEnv, key *ecdsa.PrivateKey, from, to uint64) int64 {
	t.Helper()
	h.approve(t, key, env.bondToken, env.registry, env.bond)

	reg := h.registryABI(t)
	root := crypto.Keccak256Hash([]byte("root"))
	before, err := h.client(t, env.registry).EpochCount(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.sendKey(t, key, &env.registry, nil, h.pack(t, reg, "publishIndex",
		uint64(1), from, to, [32]byte(root), [32]byte{}, "ipfs://table"))
	return int64(before)
}

// fundBond gives a key enough of the bond currency to post several bonds.
func (h *harness) fundBond(t *testing.T, env oracleEnv, key *ecdsa.PrivateKey) common.Address {
	t.Helper()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	amount := new(big.Int).Mul(env.bond, big.NewInt(10))
	h.sendFaucet(t, &env.bondToken, nil, h.pack(t, h.abiOf(t, "DemoERC20"), "mint", addr, amount))
	return addr
}

func (h *harness) approve(t *testing.T, key *ecdsa.PrivateKey, token, spender common.Address, amount *big.Int) {
	t.Helper()
	erc20, err := hintreg.ERC20ABI()
	if err != nil {
		t.Fatal(err)
	}
	h.sendKey(t, key, &token, nil, h.pack(t, erc20, "approve", spender, amount))
}

func (h *harness) client(t *testing.T, registry common.Address) *hintreg.Client {
	t.Helper()
	c, err := hintreg.NewClient(h.node, registry)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *harness) epoch(t *testing.T, registry common.Address, epochID int64) hintreg.RegistryEpoch {
	t.Helper()
	e, err := h.client(t, registry).GetEpoch(h.ctx, epochID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// simulate reports whether a registry call would revert, without paying for it.
func (h *harness) simulate(t *testing.T, registry common.Address, method string, args ...any) error {
	t.Helper()
	return h.client(t, registry).Simulate(h.ctx, h.faucet, method, args...)
}

func (h *harness) waitLiveness() {
	// Plus a margin: the contract compares against the including block's timestamp.
	time.Sleep((liveness + 2) * time.Second)
}

func (h *harness) balanceOf(t *testing.T, token, owner common.Address) *big.Int {
	t.Helper()
	erc20, err := hintreg.ERC20ABI()
	if err != nil {
		t.Fatal(err)
	}
	vals := h.call(t, token, erc20, "balanceOf", owner)
	n, ok := vals[0].(*big.Int)
	if !ok {
		t.Fatalf("balanceOf returned %T", vals[0])
	}
	return n
}

// ethBalance reads a plain ether balance. chain.Source deliberately exposes only what
// the indexer needs, so the test dials its own client for this.
func (h *harness) ethBalance(t *testing.T, owner common.Address) *big.Int {
	t.Helper()
	bal, err := h.eth.BalanceAt(h.ctx, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	return bal
}

func (h *harness) call(t *testing.T, to common.Address, parsed abi.ABI, method string, args ...any) []any {
	t.Helper()
	out, err := h.node.CallAtHead(h.ctx, ethereum.CallMsg{To: &to, Data: h.pack(t, parsed, method, args...)})
	if err != nil {
		t.Fatalf("call %s: %v", method, err)
	}
	vals, err := parsed.Unpack(method, out)
	if err != nil {
		t.Fatalf("unpack %s: %v", method, err)
	}
	return vals
}

func (h *harness) pack(t *testing.T, parsed abi.ABI, method string, args ...any) []byte {
	t.Helper()
	data, err := parsed.Pack(method, args...)
	if err != nil {
		t.Fatalf("pack %s: %v", method, err)
	}
	return data
}

func (h *harness) abiOf(t *testing.T, name string) abi.ABI {
	t.Helper()
	art, err := contracts.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func (h *harness) registryABI(t *testing.T) abi.ABI {
	t.Helper()
	return h.abiOf(t, "HintRegistry")
}

func (h *harness) deploy(t *testing.T, name string, args ...any) common.Address {
	t.Helper()
	art, err := contracts.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctor, err := parsed.Pack("", args...)
	if err != nil {
		t.Fatalf("pack %s constructor: %v", name, err)
	}
	r := h.sendFaucet(t, nil, nil, append(art.Creation(), ctor...))
	if r.ContractAddress == (common.Address{}) {
		t.Fatalf("%s deployment produced no address", name)
	}
	return r.ContractAddress
}

// fundedKey derives a deterministic test key and tops it up from the faucet.
func (h *harness) fundedKey(t *testing.T, label string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("evm-scan test " + label + t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	h.sendFaucet(t, &addr, new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18)), nil)
	return key
}

func (h *harness) sendFaucet(t *testing.T, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	t.Helper()
	gas := h.estimate(t, h.faucet, to, value, data)
	hash, err := h.node.SendUnsigned(h.ctx, h.faucet, to, value, data, gas)
	if err != nil {
		t.Fatalf("send from faucet: %v", err)
	}
	return h.receipt(t, hash)
}

func (h *harness) sendKey(t *testing.T, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	t.Helper()
	from := crypto.PubkeyToAddress(key.PublicKey)
	gas := h.estimate(t, from, to, value, data)

	nonce, err := h.node.PendingNonceAt(h.ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := h.node.SuggestGasTipCap(h.ctx)
	if err != nil {
		tip = big.NewInt(1e9)
	}
	head, err := h.node.HeaderByNumber(h.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   h.chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2))),
		Gas:       gas,
		To:        to,
		Value:     value,
		Data:      data,
	}), types.LatestSignerForChainID(h.chainID), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.node.SendTransaction(h.ctx, tx); err != nil {
		t.Fatalf("send from %s: %v", from.Hex(), err)
	}
	return h.receipt(t, tx.Hash())
}

func (h *harness) estimate(t *testing.T, from common.Address, to *common.Address, value *big.Int, data []byte) uint64 {
	t.Helper()
	gas, err := h.node.EstimateGas(h.ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data})
	if err != nil {
		t.Fatalf("estimate gas: %v", err)
	}
	return gas + gas/4
}

func (h *harness) receipt(t *testing.T, hash common.Hash) *types.Receipt {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := h.node.TransactionReceipt(h.ctx, hash)
		if err == nil {
			if r.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("transaction %s reverted", hash.Hex())
			}
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", hash.Hex())
		}
		time.Sleep(200 * time.Millisecond)
	}
}
