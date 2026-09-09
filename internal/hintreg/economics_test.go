package hintreg

import (
	"context"
	"crypto/ecdsa"
	"log/slog"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/store"
)

// TestSponsoredIndexingRoundTrip exercises the money on a real geth: a paid
// requestIndexing funds the asset, a publisher posts a commitment with coverage,
// finalizing it returns the bond, and claiming the coverage pays per block out of
// that funding. The same flow runs hermetically in registry_sim_test.go; this one
// exists to prove it against a real node and the EOA submitter.
//
// Skipped unless EVMSCAN_TEST_NODE points at a `geth --dev` node (it needs the
// node's unlocked account as a faucet):
//
//	EVMSCAN_TEST_NODE=$PWD/.devchain/geth.ipc go test ./internal/hintreg/ -run Sponsored -v
func TestSponsoredIndexingRoundTrip(t *testing.T) {
	nodeURL := os.Getenv("EVMSCAN_TEST_NODE")
	if nodeURL == "" {
		t.Skip("set EVMSCAN_TEST_NODE to a geth --dev endpoint to run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	node, err := chain.Dial(ctx, nodeURL, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer node.Close()
	chainID, err := node.ChainID(ctx)
	if err != nil {
		t.Fatalf("chain id: %v", err)
	}
	accounts, err := node.Accounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("need an unlocked dev account: %v", err)
	}
	faucet := accounts[0]

	// Balances are read with a plain client: this is a test of the contract's
	// economics, not of the indexer's node discipline.
	eth, err := ethclient.DialContext(ctx, nodeURL)
	if err != nil {
		t.Fatalf("dial ethclient: %v", err)
	}
	defer eth.Close()

	const (
		publisherBond  = 1e15
		rewardPerBlock = 1e14
		sponsor        = 1e16 // buys 100 blocks
		coverFrom      = 1
		coverTo        = 20 // 20 blocks covered -> 2e15 claimed
	)

	// ------------------------------------------------------------- deploy
	art, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctorArgs, err := regABI.Pack("", ConstructorArgs(common.Address{}, common.Address{}, faucet, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(publisherBond), ChallengeWindow: big.NewInt(1),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(rewardPerBlock),
	}, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := node.SendUnsigned(ctx, faucet, nil, nil, append(art.Creation(), ctorArgs...), 3_000_000)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	waiter := NewEOASubmitter(node, mustKey(t), chainID, time.Minute) // only used for Wait
	rcpt, err := waiter.Wait(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy receipt: %v (status %d)", err, rcpt.Status)
	}
	registry := rcpt.ContractAddress

	client, err := NewClient(node, registry)
	if err != nil {
		t.Fatal(err)
	}

	// ------------------------------------------------ fund the publisher
	pubKey := mustKey(t)
	sub := NewEOASubmitter(node, pubKey, chainID, time.Minute)
	pubAddr := sub.Sender()
	h, err = node.SendUnsigned(ctx, faucet, &pubAddr, big.NewInt(1e18), nil, 21_000)
	if err != nil {
		t.Fatalf("fund publisher: %v", err)
	}
	if _, err := waiter.Wait(ctx, h); err != nil {
		t.Fatal(err)
	}

	// -------------------------------------------- pay for indexing
	token := common.HexToAddress("0x000000000000000000000000000000000000c0de")
	data, err := regABI.Pack("requestIndexing", chainID, token, uint8(20), uint64(0))
	if err != nil {
		t.Fatal(err)
	}
	h, err = node.SendUnsigned(ctx, faucet, &registry, big.NewInt(sponsor), data, 300_000)
	if err != nil {
		t.Fatalf("requestIndexing: %v", err)
	}
	if r, err := waiter.Wait(ctx, h); err != nil || r.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("requestIndexing receipt: %v", err)
	}
	key := AssetKey(chainID, token)
	funding, err := client.Funding(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if funding.Balance.Cmp(big.NewInt(sponsor)) != 0 {
		t.Fatalf("funding after request: want %d, got %s", int64(sponsor), funding.Balance)
	}
	assets, err := client.ListAssets(ctx, 0)
	if err != nil || len(assets) != 1 || assets[0].Token != token || !assets[0].Active {
		t.Fatalf("requestIndexing should have registered the asset: %v %+v", err, assets)
	}

	// ------------------------------------------------- publish + finalize
	st := newMemStore()
	st.sets = []store.AccountAssetSet{{Account: pubAddr, Assets: []common.Address{token}}}
	st.cursors = []store.Cursor{{Address: token, BackfillDone: true, BackfillFloor: coverFrom, TailBlock: coverTo}}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pub := newPublisher(client, sub, st, log)
	pub.FinalizeSlack = 0

	e, err := pub.Build(ctx, chainID, "", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	stored, _ := st.GetEpoch(ctx, e.ID)
	if stored.Status != store.EpochPublished || stored.OnchainID == nil {
		t.Fatalf("epoch should be published: %+v", stored)
	}
	wantReward := big.NewInt(rewardPerBlock * (coverTo - coverFrom + 1))
	if stored.ExpectedRewardWei != wantReward.String() {
		t.Fatalf("expected reward: want %s, got %s", wantReward, stored.ExpectedRewardWei)
	}

	before, err := eth.BalanceAt(ctx, pubAddr, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Window is 1s; the dev chain mines every 2s, so give it a couple of blocks.
	time.Sleep(5 * time.Second)
	n, err := pub.FinalizeDue(ctx, chainID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 finalization, got %d", n)
	}
	if stored, _ = st.GetEpoch(ctx, e.ID); stored.Status != store.EpochFinalized {
		t.Fatalf("epoch should be finalized: %+v", stored)
	}

	n, err = pub.ClaimDue(ctx, chainID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 claim, got %d", n)
	}
	if stored, _ = st.GetEpoch(ctx, e.ID); stored.RewardWei != wantReward.String() {
		t.Fatalf("claimed reward: want %s, got %q", wantReward, stored.RewardWei)
	}

	after, err := eth.BalanceAt(ctx, pubAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Bond back plus the coverage reward, minus gas for finalizeIndex and
	// claimCoverage. On a dev chain gas is far below the reward, so the publisher
	// must come out ahead of bond alone.
	if after.Cmp(new(big.Int).Add(before, big.NewInt(publisherBond))) <= 0 {
		t.Fatalf("publisher should be paid bond + reward: before %s after %s", before, after)
	}
	funding, err = client.Funding(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if want := new(big.Int).Sub(big.NewInt(sponsor), wantReward); funding.Balance.Cmp(want) != 0 {
		t.Fatalf("funding after claim: want %s, got %s", want, funding.Balance)
	}
	if funding.PaidFrom != coverFrom || funding.PaidTo != coverTo {
		t.Fatalf("paid range: want [%d,%d], got [%d,%d]", coverFrom, coverTo, funding.PaidFrom, funding.PaidTo)
	}

	onchain, err := client.GetEpoch(ctx, *stored.OnchainID)
	if err != nil || onchain.Status != EpochFinalized {
		t.Fatalf("on-chain epoch should be finalized: %v %+v", err, onchain)
	}
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
