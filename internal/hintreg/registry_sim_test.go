package hintreg

import (
	"context"
	"crypto/ecdsa"
	"log/slog"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/merkle"
	"github.com/Skanislav/evm-scan/internal/store"
)

var merkleBuild = merkle.Build

// sim is a self-mining chain with one funded key, wearing both hats the publisher
// needs: it is the Submitter that carries transactions and the head caller behind
// the registry Client. No geth, no network.
type sim struct {
	t       *testing.T
	backend *simulated.Backend
	client  simulated.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	chainID *big.Int
}

func newSim(t *testing.T) *sim {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	backend := simulated.NewBackend(types.GenesisAlloc{from: {Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))}})
	t.Cleanup(func() { backend.Close() })
	s := &sim{t: t, backend: backend, client: backend.Client(), key: key, from: from}
	s.chainID, err = s.client.ChainID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *sim) Sender() common.Address { return s.from }

func (s *sim) Submit(ctx context.Context, to common.Address, value *big.Int, data []byte) (common.Hash, error) {
	return s.sendTx(ctx, &to, value, data)
}

func (s *sim) Wait(ctx context.Context, ref common.Hash) (*types.Receipt, error) {
	return s.client.TransactionReceipt(ctx, ref)
}

func (s *sim) CallAtHead(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	return s.client.CallContract(ctx, msg, nil)
}

// sendTx signs, sends and mines one transaction. A nil `to` deploys.
func (s *sim) sendTx(ctx context.Context, to *common.Address, value *big.Int, data []byte) (common.Hash, error) {
	if value == nil {
		value = new(big.Int)
	}
	nonce, err := s.client.PendingNonceAt(ctx, s.from)
	if err != nil {
		return common.Hash{}, err
	}
	gasPrice, err := s.client.SuggestGasPrice(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	gas, err := s.client.EstimateGas(ctx, ethereum.CallMsg{From: s.from, To: to, Value: value, Data: data})
	if err != nil {
		return common.Hash{}, err
	}
	tx := types.MustSignNewTx(s.key, types.LatestSignerForChainID(s.chainID), &types.LegacyTx{
		Nonce: nonce, GasPrice: gasPrice, Gas: gas + gas/2, To: to, Value: value, Data: data,
	})
	if err := s.client.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, err
	}
	s.backend.Commit()
	return tx.Hash(), nil
}

func (s *sim) mustSend(ctx context.Context, to common.Address, value *big.Int, data []byte) *types.Receipt {
	s.t.Helper()
	h, err := s.sendTx(ctx, &to, value, data)
	if err != nil {
		s.t.Fatalf("send: %v", err)
	}
	r, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || r.Status != types.ReceiptStatusSuccessful {
		s.t.Fatalf("tx %s failed: %v", h.Hex(), err)
	}
	return r
}

func (s *sim) balance(ctx context.Context, a common.Address) *big.Int {
	s.t.Helper()
	b, err := s.client.BalanceAt(ctx, a, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return b
}

func (s *sim) chainTime(ctx context.Context) time.Time {
	s.t.Helper()
	h, err := s.client.HeaderByNumber(ctx, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return time.Unix(int64(h.Time), 0)
}

// pastWindow moves the chain clock past a challenge window of `window` seconds.
func (s *sim) pastWindow(window time.Duration) {
	if err := s.backend.AdjustTime(window + 2*time.Second); err != nil {
		s.t.Fatal(err)
	}
	s.backend.Commit()
}

// TestCoverageRewardsOnSimulatedChain runs the whole economic loop against the real
// contract: a request funds an asset, the publisher's epoch declares coverage,
// finalization returns the bond, the claim pays per newly covered block, and a
// later epoch is only paid for the blocks it adds.
func TestCoverageRewardsOnSimulatedChain(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()

	const (
		publisherBond  = 1e15
		rewardPerBlock = 1e12
		fundedBlocks   = 100
		window         = 5 * time.Second
	)
	funding := big.NewInt(rewardPerBlock * fundedBlocks)

	// ------------------------------------------------------------- deploy
	art, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctor, err := regABI.Pack("", ConstructorArgs(common.Address{}, common.Address{}, s.from, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(publisherBond), ChallengeWindow: big.NewInt(int64(window / time.Second)),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(rewardPerBlock),
	}, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.sendTx(ctx, nil, nil, append(art.Creation(), ctor...))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rcpt, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy receipt: %v", err)
	}
	registry := rcpt.ContractAddress
	client, err := NewClient(s, registry)
	if err != nil {
		t.Fatal(err)
	}

	// ------------------------------------------- one funded, one free asset
	funded := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	free := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	data, _ := regABI.Pack("requestIndexing", chainID, funded, uint8(20), uint64(0))
	s.mustSend(ctx, registry, funding, data)
	data, _ = regABI.Pack("registerAsset", chainID, free, uint8(20), uint64(0))
	s.mustSend(ctx, registry, nil, data)

	fundedKey := AssetKey(chainID, funded)
	if f, _ := client.Funding(ctx, fundedKey); f.Balance.Cmp(funding) != 0 || f.PaidTo != 0 {
		t.Fatalf("funding after request: %+v", f)
	}
	if got := s.balance(ctx, registry); got.Cmp(funding) != 0 {
		t.Fatalf("registry should hold the funding: %s", got)
	}

	// ------------------------------------------------------ the publisher
	st := newMemStore()
	st.from, st.to = 10, 20
	st.sets = []store.AccountAssetSet{{Account: s.from, Assets: []common.Address{funded, free}}}
	st.cursors = []store.Cursor{
		{Address: funded, BackfillDone: true, BackfillFloor: 5, TailBlock: 20}, // [5,20]: 16 blocks
		{Address: free, BackfillDone: true, BackfillFloor: 12, TailBlock: 18},  // [12,18]: unfunded
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pub := newPublisher(client, s, st, log)
	pub.FinalizeSlack = 0
	// The publisher compares its wall clock with the chain's deadlines; here the
	// chain's clock is the only one that moves, so read it.
	pub.now = func() time.Time { return s.chainTime(ctx) }

	// Coverage is priced by the registry: only the funded asset counts.
	e1, err := pub.Build(ctx, chainID, "", false)
	if err != nil {
		t.Fatalf("build 1: %v", err)
	}
	if want := big.NewInt(rewardPerBlock * 16); e1.ExpectedRewardWei != want.String() {
		t.Fatalf("expected reward: want %s, got %s", want, e1.ExpectedRewardWei)
	}

	// Publishing locks the bond in the registry.
	if _, err := pub.Publish(ctx, e1.ID); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if got, want := s.balance(ctx, registry), new(big.Int).Add(funding, big.NewInt(publisherBond)); got.Cmp(want) != 0 {
		t.Fatalf("registry after publish: want %s, got %s", want, got)
	}
	onchain, err := client.GetEpoch(ctx, *mustEpoch(t, st, e1.ID).OnchainID)
	if err != nil || onchain.CoverageRoot != e1.CoverageRoot || onchain.Root != e1.MerkleRoot {
		t.Fatalf("on-chain epoch does not carry our roots: %v %+v", err, onchain)
	}

	// Too early: the window is open, nothing finalizes, nothing is claimable.
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 0 {
		t.Fatalf("finalize inside window: n=%d err=%v", n, err)
	}
	if n, err := pub.ClaimDue(ctx, chainID); err != nil || n != 0 {
		t.Fatalf("claim before finalize: n=%d err=%v", n, err)
	}

	// Past the window: the bond comes back, then the claim pays 16 blocks.
	s.pastWindow(window)
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("finalize 1: n=%d err=%v", n, err)
	}
	if got, want := s.balance(ctx, registry), funding; got.Cmp(want) != 0 {
		t.Fatalf("registry after finalize should hold only funding: want %s, got %s", want, got)
	}
	if n, err := pub.ClaimDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("claim 1: n=%d err=%v", n, err)
	}
	paid1 := big.NewInt(rewardPerBlock * 16)
	if got := mustEpoch(t, st, e1.ID); got.RewardWei != paid1.String() || got.ClaimTx == nil {
		t.Fatalf("epoch 1 claim: %+v", got)
	}
	f, _ := client.Funding(ctx, fundedKey)
	if f.Balance.Cmp(new(big.Int).Sub(funding, paid1)) != 0 || f.PaidFrom != 5 || f.PaidTo != 20 {
		t.Fatalf("funding after claim 1: %+v", f)
	}
	if got, want := s.balance(ctx, registry), new(big.Int).Sub(funding, paid1); got.Cmp(want) != 0 {
		t.Fatalf("registry after claim 1: want %s, got %s", want, got)
	}

	// The same range is worth nothing now; a wider one is worth only its extension.
	if q, _ := client.Claimable(ctx, fundedKey, 5, 20); q.Sign() != 0 {
		t.Fatalf("replayed range should quote zero, got %s", q)
	}
	if q, want := mustClaimable(t, client, fundedKey, 1, 30), big.NewInt(rewardPerBlock*14); q.Cmp(want) != 0 {
		t.Fatalf("extension quote: want %s, got %s", want, q)
	}

	// ----------------------------------------- epoch 2 extends coverage
	st.cursors[0].TailBlock = 30
	st.to = 30
	e2, err := pub.Build(ctx, chainID, "", false)
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	if want := big.NewInt(rewardPerBlock * 10); e2.ExpectedRewardWei != want.String() {
		t.Fatalf("epoch 2 expected reward: want %s, got %s", want, e2.ExpectedRewardWei)
	}
	if _, err := pub.Publish(ctx, e2.ID); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	s.pastWindow(window)
	if n, err := pub.FinalizeDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("finalize 2: n=%d err=%v", n, err)
	}
	if n, err := pub.ClaimDue(ctx, chainID); err != nil || n != 1 {
		t.Fatalf("claim 2: n=%d err=%v", n, err)
	}
	paid2 := big.NewInt(rewardPerBlock * 10)
	if got := mustEpoch(t, st, e2.ID); got.RewardWei != paid2.String() {
		t.Fatalf("epoch 2 claim: %+v", got)
	}
	f, _ = client.Funding(ctx, fundedKey)
	if f.PaidFrom != 5 || f.PaidTo != 30 {
		t.Fatalf("paid range after epoch 2: [%d,%d]", f.PaidFrom, f.PaidTo)
	}

	// --------------------------- replaying epoch 1's claim pays nothing
	cov, _ := st.EpochCoverage(ctx, e1.ID)
	claims := claimsFor(t, cov)
	data, err = regABI.Pack("claimCoverage", big.NewInt(*mustEpoch(t, st, e1.ID).OnchainID), claims)
	if err != nil {
		t.Fatal(err)
	}
	before := s.balance(ctx, registry)
	s.mustSend(ctx, registry, nil, data)
	if got := s.balance(ctx, registry); got.Cmp(before) != 0 {
		t.Fatalf("replayed claim moved money: before %s after %s", before, got)
	}
	// The replay also carried the unfunded asset's leaf. Paying nothing for it must
	// not mark its blocks paid, or funding it later could never pay for them.
	if ff, _ := client.Funding(ctx, AssetKey(chainID, free)); ff.PaidTo != 0 {
		t.Fatalf("a zero-reward claim moved the paid range: %+v", ff)
	}

	// ------------------------------- funding is a cap, not a promise
	// Top up the free asset with 3 blocks' worth and cover 7: it pays out 3.
	data, _ = regABI.Pack("fundAsset", [32]byte(AssetKey(chainID, free)))
	s.mustSend(ctx, registry, big.NewInt(rewardPerBlock*3), data)
	if q := mustClaimable(t, client, AssetKey(chainID, free), 12, 18); q.Cmp(big.NewInt(rewardPerBlock*3)) != 0 {
		t.Fatalf("capped quote: want %d, got %s", int64(rewardPerBlock*3), q)
	}
	// Everything the registry paid out went to the publisher.
	paidOut := new(big.Int).Add(paid1, paid2)
	if got, want := s.balance(ctx, registry), new(big.Int).Sub(new(big.Int).Add(funding, big.NewInt(rewardPerBlock*3)), paidOut); got.Cmp(want) != 0 {
		t.Fatalf("registry balance: want %s, got %s", want, got)
	}
}

func mustEpoch(t *testing.T, st *memStore, id int64) store.Epoch {
	t.Helper()
	e, err := st.GetEpoch(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mustClaimable(t *testing.T, c *Client, key common.Hash, from, to uint64) *big.Int {
	t.Helper()
	q, err := c.Claimable(context.Background(), key, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// claimsFor builds the full claim set for an epoch's coverage, proofs included.
func claimsFor(t *testing.T, cov []store.EpochCoverage) []coverageClaim {
	t.Helper()
	leaves := make([]common.Hash, len(cov))
	for i, c := range cov {
		leaves[i] = c.Leaf
	}
	tree := merkleBuild(leaves)
	var out []coverageClaim
	for i, c := range cov {
		proof, err := tree.Proof(i)
		if err != nil {
			t.Fatal(err)
		}
		cl := coverageClaim{Key: c.RegistryKey, FromBlock: c.FromBlock, ToBlock: c.ToBlock}
		for _, h := range proof {
			cl.Proof = append(cl.Proof, h)
		}
		out = append(out, cl)
	}
	return out
}

// A revoked asset keeps its place in the key list, so registering it again must not
// append a second entry: `listAssets` is how an indexer bootstraps its scan set, and
// a duplicate there makes it scan the same contract twice.
func TestReRegisterAfterRevokeKeepsOneKey(t *testing.T) {
	ctx := context.Background()
	s := newSim(t)
	chainID := s.chainID.Uint64()

	art, err := contracts.Load("HintRegistry")
	if err != nil {
		t.Fatal(err)
	}
	regABI, err := art.Parsed()
	if err != nil {
		t.Fatal(err)
	}
	ctor, err := regABI.Pack("", ConstructorArgs(common.Address{}, common.Address{}, s.from, Economics{
		AssetBond: big.NewInt(0), PublisherBond: big.NewInt(1e15), ChallengeWindow: big.NewInt(5),
		MinFunding: big.NewInt(0), RewardPerBlock: big.NewInt(1e12),
	}, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.sendTx(ctx, nil, nil, append(art.Creation(), ctor...))
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	rcpt, err := s.client.TransactionReceipt(ctx, h)
	if err != nil || rcpt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("deploy receipt: %v", err)
	}
	registry := rcpt.ContractAddress

	token := common.HexToAddress("0x00000000000000000000000000000000000000cc")
	key := AssetKey(chainID, token)

	data, _ := regABI.Pack("registerAsset", chainID, token, uint8(20), uint64(0))
	s.mustSend(ctx, registry, nil, data)
	data, _ = regABI.Pack("revokeAsset", key)
	s.mustSend(ctx, registry, nil, data)
	data, _ = regABI.Pack("registerAsset", chainID, token, uint8(20), uint64(0))
	s.mustSend(ctx, registry, nil, data)

	data, _ = regABI.Pack("assetCount")
	out, err := s.CallAtHead(ctx, ethereum.CallMsg{To: &registry, Data: data})
	if err != nil {
		t.Fatalf("assetCount: %v", err)
	}
	vals, err := regABI.Unpack("assetCount", out)
	if err != nil {
		t.Fatal(err)
	}
	if n := vals[0].(*big.Int); n.Int64() != 1 {
		t.Fatalf("assetCount after register/revoke/register = %s, want 1", n)
	}
}
