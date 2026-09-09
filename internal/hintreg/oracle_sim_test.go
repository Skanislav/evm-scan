package hintreg

import (
	"context"
	"log/slog"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/store"
)

// TestOracleModePublisherOnSimulatedChain runs the publisher against a registry in
// oracle mode: the bond is an ERC-20 the publisher must approve, publishIndex asserts
// to the (mock) oracle, finalizeIndex settles the assertion once liveness has passed,
// and the coverage claim still pays afterwards. This is the path the hosted
// deployment is meant to run, so it is exercised without a node.
func TestOracleModePublisherOnSimulatedChain(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()

	const (
		publisherBond  = 1_000_000 // bond-token units
		rewardPerBlock = 1e12
		fundedBlocks   = 100
		liveness       = 5 * time.Second
	)
	funding := big.NewInt(rewardPerBlock * fundedBlocks)

	// ------------------------------------------------------------- deploy
	bondToken := s.deployContract(t, ctx, "DemoERC20", "Bond Token", "BOND")
	// The sim key stands in for UMA's voters; nothing here is disputed.
	oracle := s.deployContract(t, ctx, "MockOptimisticOracleV3", s.from, big.NewInt(0))

	art, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	registry := s.deployContract(t, ctx, "HintRegistry", ConstructorArgs(oracle, bondToken, common.Address{}, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(publisherBond), ChallengeWindow: big.NewInt(int64(liveness / time.Second)),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(rewardPerBlock),
	}, []string{"https://gw.example/{sender}/{data}.json"})...)
	client, err := NewClient(s, registry)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := client.Mode(ctx)
	if err != nil || !mode.OracleMode() || mode.BondCurrency != bondToken {
		t.Fatalf("mode = %+v (%v), want oracle mode bonded in %s", mode, err, bondToken.Hex())
	}
	if urls, _ := client.Gateways(ctx); len(urls) != 1 {
		t.Fatalf("constructor gateways not stored: %v", urls)
	}

	// The publisher holds bond tokens but has approved nothing yet.
	erc20, _ := ERC20ABI()
	demoArt, _ := contracts.Load("DemoERC20")
	demoABI, _ := demoArt.Parsed()
	data, _ := demoABI.Pack("mint", s.from, big.NewInt(publisherBond*3))
	s.mustSend(ctx, bondToken, nil, data)

	asset := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	data, _ = regABI.Pack("requestIndexing", chainID, asset, uint8(20), uint64(0))
	s.mustSend(ctx, registry, funding, data)

	// ------------------------------------------------------ the publisher
	st := newMemStore()
	st.from, st.to = 10, 20
	st.sets = []store.AccountAssetSet{{Account: s.from, Assets: []common.Address{asset}}}
	st.cursors = []store.Cursor{{Address: asset, BackfillDone: true, BackfillFloor: 5, TailBlock: 20}}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pub := newPublisher(client, s, st, log)
	pub.FinalizeSlack = 0
	pub.now = func() time.Time { return s.chainTime(ctx) }

	e, err := pub.Build(ctx, chainID, "ipfs://table", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Publish approves the bond, then asserts. No ether moves.
	if _, err := pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := s.balance(ctx, registry); got.Cmp(funding) != 0 {
		t.Fatalf("registry ether should be funding only, got %s", got)
	}
	local := mustEpoch(t, st, e.ID)
	onchain, err := client.GetEpoch(ctx, *local.OnchainID)
	if err != nil {
		t.Fatal(err)
	}
	if onchain.AssertionID == (common.Hash{}) || onchain.Status != EpochProposed || onchain.CoverageRoot != e.CoverageRoot {
		t.Fatalf("on-chain epoch after oracle publish: %+v", onchain)
	}
	if bal := s.erc20Balance(t, ctx, erc20, bondToken, s.from); bal.Cmp(big.NewInt(publisherBond*2)) != 0 {
		t.Fatalf("bond not escrowed: publisher holds %s", bal)
	}

	// Still live: finalizeIndex would revert, so FinalizeDue must not send it.
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 0 {
		t.Fatalf("finalize while live: n=%d err=%v", n, err)
	}

	// Past liveness: settle returns the bond and finalizes through the callback.
	s.pastWindow(liveness)
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("finalize after liveness: n=%d err=%v", n, err)
	}
	if bal := s.erc20Balance(t, ctx, erc20, bondToken, s.from); bal.Cmp(big.NewInt(publisherBond*3)) != 0 {
		t.Fatalf("bond not returned: publisher holds %s", bal)
	}
	if got := mustEpoch(t, st, e.ID); got.Status != store.EpochFinalized {
		t.Fatalf("local status after settle: %s", got.Status)
	}

	// Coverage pays exactly as in local-arbiter mode.
	if n, err := pub.ClaimDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("claim: n=%d err=%v", n, err)
	}
	if got := mustEpoch(t, st, e.ID); got.RewardWei != big.NewInt(rewardPerBlock*16).String() {
		t.Fatalf("reward: %+v", got)
	}

	// A second publish finds the allowance spent and approves again.
	st.cursors[0].TailBlock, st.to = 30, 30
	e2, err := pub.Build(ctx, chainID, "ipfs://table2", false)
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if _, err := pub.Publish(ctx, e2.ID); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	if bal := s.erc20Balance(t, ctx, erc20, bondToken, s.from); bal.Cmp(big.NewInt(publisherBond*2)) != 0 {
		t.Fatalf("second bond not escrowed: publisher holds %s", bal)
	}
}

// deployContract deploys a compiled artifact with the given constructor args.
func (s *sim) deployContract(t *testing.T, ctx context.Context, name string, args ...any) common.Address {
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
	h, err := s.sendTx(ctx, nil, nil, append(art.Creation(), ctor...))
	if err != nil {
		t.Fatalf("deploy %s: %v", name, err)
	}
	rcpt, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy %s receipt: %v", name, err)
	}
	return rcpt.ContractAddress
}

func (s *sim) erc20Balance(t *testing.T, ctx context.Context, erc20 interface {
	Pack(string, ...any) ([]byte, error)
	Unpack(string, []byte) ([]any, error)
}, token, owner common.Address) *big.Int {
	t.Helper()
	in, _ := erc20.Pack("balanceOf", owner)
	out, err := s.client.CallContract(ctx, callMsg(token, in), nil)
	if err != nil {
		t.Fatal(err)
	}
	vals, err := erc20.Unpack("balanceOf", out)
	if err != nil {
		t.Fatal(err)
	}
	return vals[0].(*big.Int)
}
