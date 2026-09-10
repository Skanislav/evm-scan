package hintreg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Skanislav/evm-scan/contracts"
	"github.com/Skanislav/evm-scan/internal/store"
)

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

// memStore is an in-memory epochStore.
type memStore struct {
	epochs   map[int64]*store.Epoch
	coverage map[int64][]store.EpochCoverage
	nextID   int64
	sets     []store.AccountAssetSet
	cursors  []store.Cursor
	from     uint64
	to       uint64
}

func newMemStore() *memStore {
	return &memStore{
		epochs: map[int64]*store.Epoch{}, coverage: map[int64][]store.EpochCoverage{},
		nextID: 1, from: 10, to: 20,
	}
}

func (m *memStore) CoverageRange(context.Context, uint64) (uint64, uint64, error) {
	return m.from, m.to, nil
}

func (m *memStore) ListCursors(context.Context, uint64) ([]store.Cursor, error) {
	return m.cursors, nil
}

func (m *memStore) SnapshotIndex(context.Context, uint64, uint64) ([]store.AccountAssetSet, error) {
	return m.sets, nil
}

func (m *memStore) CreateEpoch(_ context.Context, e store.Epoch, leaves []store.EpochLeaf, coverage []store.EpochCoverage) (int64, error) {
	id := m.nextID
	m.nextID++
	e.ID = id
	e.Status = store.EpochBuilt
	e.LeafCount = int64(len(leaves))
	m.epochs[id] = &e
	m.coverage[id] = coverage
	return id, nil
}

func (m *memStore) EpochCoverage(_ context.Context, id int64) ([]store.EpochCoverage, error) {
	return m.coverage[id], nil
}

func (m *memStore) MarkClaimed(_ context.Context, id int64, tx common.Hash, rewardWei string) error {
	e := m.epochs[id]
	now := time.Now()
	e.ClaimedAt = &now
	e.RewardWei = rewardWei
	if tx != (common.Hash{}) {
		e.ClaimTx = &tx
	}
	return nil
}

func (m *memStore) UnclaimedFinalized(context.Context, uint64) ([]store.Epoch, error) {
	return m.byStatus(func(e *store.Epoch) bool {
		return e.Status == store.EpochFinalized && e.OnchainID != nil && e.ClaimedAt == nil
	}), nil
}

func (m *memStore) GetEpoch(_ context.Context, id int64) (store.Epoch, error) {
	e, ok := m.epochs[id]
	if !ok {
		return store.Epoch{}, store.ErrNotFound
	}
	return *e, nil
}

func (m *memStore) MarkSubmitted(_ context.Context, id int64, ref common.Hash) error {
	e := m.epochs[id]
	e.Status = store.EpochSubmitted
	e.SubmissionRef = &ref
	now := time.Now()
	e.SubmittedAt = &now
	return nil
}

func (m *memStore) MarkPublished(_ context.Context, id int64, onchainID int64, txHash common.Hash) error {
	e := m.epochs[id]
	e.Status = store.EpochPublished
	e.OnchainID = &onchainID
	e.TxHash = &txHash
	return nil
}

func (m *memStore) SetEpochStatus(_ context.Context, id int64, status string) error {
	m.epochs[id].Status = status
	return nil
}

func (m *memStore) SetEpochURI(_ context.Context, id int64, uri string) error {
	m.epochs[id].URI = uri
	return nil
}

func (m *memStore) byStatus(pred func(*store.Epoch) bool) []store.Epoch {
	var out []store.Epoch
	for id := int64(1); id < m.nextID; id++ {
		if e := m.epochs[id]; e != nil && pred(e) {
			out = append(out, *e)
		}
	}
	return out
}

func (m *memStore) PendingSubmissions(context.Context, uint64) ([]store.Epoch, error) {
	return m.byStatus(func(e *store.Epoch) bool { return e.Status == store.EpochSubmitted }), nil
}

func (m *memStore) PublishedUnfinalized(context.Context, uint64) ([]store.Epoch, error) {
	return m.byStatus(func(e *store.Epoch) bool {
		return e.Status == store.EpochPublished && e.OnchainID != nil
	}), nil
}

func (m *memStore) LatestCommittedRoots(context.Context, uint64) (common.Hash, common.Hash, bool, error) {
	for id := m.nextID - 1; id >= 1; id-- {
		e := m.epochs[id]
		switch e.Status {
		case store.EpochSubmitted, store.EpochPublished, store.EpochFinalized:
			return e.MerkleRoot, e.CoverageRoot, true, nil
		}
	}
	return common.Hash{}, common.Hash{}, false, nil
}

// memSub is a Submitter whose Wait behaviour the test controls.
type memSub struct {
	sender  common.Address
	submits int
	waits   int
	// calls records every Submit's calldata selector, so a test can tell
	// publishIndex from finalizeIndex.
	calls []string
	wait  func(ref common.Hash) (*types.Receipt, error)
}

func (s *memSub) Sender() common.Address { return s.sender }

func (s *memSub) Submit(_ context.Context, _ common.Address, _ *big.Int, data []byte) (common.Hash, error) {
	s.submits++
	s.calls = append(s.calls, common.Bytes2Hex(data[:4]))
	return common.BigToHash(big.NewInt(int64(s.submits))), nil
}

func (s *memSub) Wait(_ context.Context, ref common.Hash) (*types.Receipt, error) {
	s.waits++
	return s.wait(ref)
}

// memReg is a registryReader with a scripted on-chain view.
type memReg struct {
	abi    abi.ABI
	addr   common.Address
	bond   *big.Int
	epochs map[int64]RegistryEpoch
	// quote is what Claimable answers for any key; nil answers zero.
	quote *big.Int
	// oracle and currency, when set, put the fake registry in oracle mode;
	// allowance is what Allowance reports (nil reads as zero) and settleable
	// is which on-chain epochs finalizeIndex would not revert for.
	oracle, currency common.Address
	allowance        *big.Int
	settleable       map[int64]bool
}

func (r *memReg) Claimable(context.Context, common.Hash, uint64, uint64) (*big.Int, error) {
	if r.quote == nil {
		return new(big.Int), nil
	}
	return new(big.Int).Set(r.quote), nil
}

func (r *memReg) Address() common.Address { return r.addr }
func (r *memReg) ABI() abi.ABI            { return r.abi }

// Mode reports a local-arbiter registry unless the test set an oracle.
func (r *memReg) Mode(context.Context) (Mode, error) {
	return Mode{
		Oracle: r.oracle, BondCurrency: r.currency, Arbiter: r.addr,
		PublisherBond: r.bond, AssetBond: new(big.Int), ChallengeWindow: 60,
	}, nil
}

func (r *memReg) Allowance(context.Context, common.Address, common.Address, common.Address) (*big.Int, error) {
	if r.allowance == nil {
		return new(big.Int), nil
	}
	return r.allowance, nil
}

// Simulate answers with the scripted verdict for the epoch id being finalized.
func (r *memReg) Simulate(_ context.Context, _ common.Address, method string, args ...any) error {
	if method == "finalizeIndex" && r.settleable != nil {
		if id, ok := args[0].(*big.Int); ok && !r.settleable[id.Int64()] {
			return errors.New("execution reverted")
		}
	}
	return nil
}
func (r *memReg) GetEpoch(_ context.Context, id int64) (RegistryEpoch, error) {
	e, ok := r.epochs[id]
	if !ok {
		return RegistryEpoch{}, fmt.Errorf("no epoch %d", id)
	}
	return e, nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

type fixture struct {
	st  *memStore
	sub *memSub
	reg *memReg
	pub *Publisher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	parsed, err := contracts.HintRegistryABI()
	if err != nil {
		t.Fatalf("load ABI: %v", err)
	}
	st := newMemStore()
	st.sets = []store.AccountAssetSet{
		{Account: common.HexToAddress("0x1"), Assets: []common.Address{common.HexToAddress("0xa")}},
		{Account: common.HexToAddress("0x2"), Assets: []common.Address{common.HexToAddress("0xa"), common.HexToAddress("0xb")}},
	}
	st.cursors = []store.Cursor{
		{Address: common.HexToAddress("0xa"), BackfillNext: 4, BackfillDone: false, TailBlock: 25},
		{Address: common.HexToAddress("0xb"), BackfillDone: true, BackfillFloor: 12, TailBlock: 18},
		{Address: common.HexToAddress("0xc"), BackfillNext: 30, TailBlock: 0}, // nothing scanned
	}
	reg := &memReg{abi: parsed, addr: common.HexToAddress("0xfeed"), bond: big.NewInt(0), epochs: map[int64]RegistryEpoch{}}
	sub := &memSub{sender: common.HexToAddress("0xbeef")}
	// Default: every submission lands and published epoch id 7.
	sub.wait = func(ref common.Hash) (*types.Receipt, error) { return publishedReceipt(reg, ref, 7), nil }

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	pub := newPublisher(reg, sub, st, log)
	return &fixture{st: st, sub: sub, reg: reg, pub: pub}
}

// publishedReceipt fakes a successful publishIndex receipt carrying IndexPublished.
func publishedReceipt(reg *memReg, ref common.Hash, epochID int64) *types.Receipt {
	ev := reg.abi.Events["IndexPublished"]
	return &types.Receipt{
		Status: types.ReceiptStatusSuccessful,
		TxHash: ref,
		Logs: []*types.Log{{
			Address: reg.addr,
			Topics:  []common.Hash{ev.ID, common.BigToHash(big.NewInt(epochID))},
		}},
	}
}

func selector(t *testing.T, parsed abi.ABI, method string) string {
	t.Helper()
	m, ok := parsed.Methods[method]
	if !ok {
		t.Fatalf("ABI has no %s", method)
	}
	return common.Bytes2Hex(m.ID)
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

func TestBuildRefusesUnchangedRootUntilForced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	e, err := f.pub.Build(ctx, 1, "", false)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if _, err := f.pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := f.pub.Build(ctx, 1, "", false); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("second build: want ErrUnchanged, got %v", err)
	}
	if len(f.st.epochs) != 1 {
		t.Fatalf("unchanged build must not insert a row; have %d", len(f.st.epochs))
	}

	if _, err := f.pub.Build(ctx, 1, "", true); err != nil {
		t.Fatalf("forced build: %v", err)
	}

	// A different index is a different root and builds without force.
	f.st.sets = append(f.st.sets, store.AccountAssetSet{
		Account: common.HexToAddress("0x3"), Assets: []common.Address{common.HexToAddress("0xc")},
	})
	if _, err := f.pub.Build(ctx, 1, "", false); err != nil {
		t.Fatalf("changed build: %v", err)
	}
}

func TestPublishRecordsReferenceBeforeWaitingAndResumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The receipt never shows up the first time round.
	f.sub.wait = func(common.Hash) (*types.Receipt, error) { return nil, errors.New("timed out") }

	e, err := f.pub.Build(ctx, 1, "", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := f.pub.Publish(ctx, e.ID); err == nil {
		t.Fatal("publish should surface the wait failure")
	}

	got, _ := f.st.GetEpoch(ctx, e.ID)
	if got.Status != store.EpochSubmitted || got.SubmissionRef == nil {
		t.Fatalf("after a failed wait the epoch must be submitted with a ref; got %+v", got)
	}

	// Building again while one is in flight is refused: the root is "committed".
	if _, err := f.pub.Build(ctx, 1, "", false); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("build during in-flight submission: want ErrUnchanged, got %v", err)
	}

	// A restart resumes the wait and does not submit a second time.
	f.sub.wait = func(ref common.Hash) (*types.Receipt, error) { return publishedReceipt(f.reg, ref, 3), nil }
	pending, err := f.pub.ResumePending(ctx, 1)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if pending != 0 {
		t.Fatalf("resume left %d pending", pending)
	}
	got, _ = f.st.GetEpoch(ctx, e.ID)
	if got.Status != store.EpochPublished || got.OnchainID == nil || *got.OnchainID != 3 {
		t.Fatalf("resumed epoch should be published as on-chain 3; got %+v", got)
	}
	if f.sub.submits != 1 {
		t.Fatalf("resume must not resubmit; submits=%d", f.sub.submits)
	}
}

func TestResumePendingWritesOffStaleSubmissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pub.StaleAfter = 10 * time.Minute

	e, _ := f.pub.Build(ctx, 1, "", false)
	_ = f.st.MarkSubmitted(ctx, e.ID, common.HexToHash("0xabc"))
	old := time.Now().Add(-time.Hour)
	f.st.epochs[e.ID].SubmittedAt = &old

	f.sub.wait = func(common.Hash) (*types.Receipt, error) {
		t.Fatal("a stale submission must not be waited on")
		return nil, nil
	}
	if _, err := f.pub.ResumePending(ctx, 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got, _ := f.st.GetEpoch(ctx, e.ID); got.Status != store.EpochFailed {
		t.Fatalf("stale submission should be failed; got %s", got.Status)
	}

	// A failed epoch no longer counts as committed, so the same root builds again.
	if _, err := f.pub.Build(ctx, 1, "", false); err != nil {
		t.Fatalf("rebuild after failure: %v", err)
	}
}

func TestPublishRevertMarksFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sub.wait = func(ref common.Hash) (*types.Receipt, error) {
		return &types.Receipt{Status: types.ReceiptStatusFailed, TxHash: ref}, nil
	}

	e, _ := f.pub.Build(ctx, 1, "", false)
	if _, err := f.pub.Publish(ctx, e.ID); err == nil {
		t.Fatal("reverted publish should error")
	}
	if got, _ := f.st.GetEpoch(ctx, e.ID); got.Status != store.EpochFailed {
		t.Fatalf("reverted publish should be failed; got %s", got.Status)
	}
}

func TestFinalizeDue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pub.FinalizeSlack = 0
	now := time.Unix(1_000_000, 0)
	f.pub.now = func() time.Time { return now }

	publish := func(onchainID int64) int64 {
		t.Helper()
		f.sub.wait = func(ref common.Hash) (*types.Receipt, error) { return publishedReceipt(f.reg, ref, onchainID), nil }
		e, err := f.pub.Build(ctx, 1, "", true)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if _, err := f.pub.Publish(ctx, e.ID); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return e.ID
	}

	stillOpen := publish(1)
	f.reg.epochs[1] = RegistryEpoch{Status: EpochProposed, ChallengeDeadline: uint64(now.Unix()) + 60}
	due := publish(2)
	f.reg.epochs[2] = RegistryEpoch{Status: EpochProposed, ChallengeDeadline: uint64(now.Unix()) - 1}
	alreadyFinal := publish(3)
	f.reg.epochs[3] = RegistryEpoch{Status: EpochFinalized}
	rejected := publish(4)
	f.reg.epochs[4] = RegistryEpoch{Status: EpochRejected}
	challenged := publish(5)
	f.reg.epochs[5] = RegistryEpoch{Status: EpochChallenged, ChallengeDeadline: uint64(now.Unix()) - 1}

	submitsBefore := f.sub.submits
	f.sub.wait = func(ref common.Hash) (*types.Receipt, error) {
		return &types.Receipt{Status: types.ReceiptStatusSuccessful, TxHash: ref}, nil
	}
	n, err := f.pub.FinalizeDue(ctx, 1)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if n != 1 {
		t.Fatalf("want exactly one finalization, got %d", n)
	}
	if f.sub.submits != submitsBefore+1 {
		t.Fatalf("want one finalizeIndex submission, got %d", f.sub.submits-submitsBefore)
	}
	if last := f.sub.calls[len(f.sub.calls)-1]; last != selector(t, f.reg.abi, "finalizeIndex") {
		t.Fatalf("last call should be finalizeIndex, got selector %s", last)
	}

	want := map[int64]string{
		stillOpen:    store.EpochPublished,
		due:          store.EpochFinalized,
		alreadyFinal: store.EpochFinalized,
		rejected:     store.EpochRejected,
		challenged:   store.EpochPublished,
	}
	for id, status := range want {
		if got, _ := f.st.GetEpoch(ctx, id); got.Status != status {
			t.Errorf("epoch %d: want %s, got %s", id, status, got.Status)
		}
	}
}

func TestEpochIDFromReceiptIgnoresOtherContracts(t *testing.T) {
	f := newFixture(t)
	ev := f.reg.abi.Events["IndexPublished"]
	r := &types.Receipt{Logs: []*types.Log{
		{Address: common.HexToAddress("0xdead"), Topics: []common.Hash{ev.ID, common.BigToHash(big.NewInt(99))}},
		{Address: f.reg.addr, Topics: []common.Hash{ev.ID, common.BigToHash(big.NewInt(4))}},
	}}
	id, err := f.pub.epochIDFromReceipt(r)
	if err != nil || id != 4 {
		t.Fatalf("want epoch 4 from our registry's log, got %d, %v", id, err)
	}
}

func TestBuildCoverageFollowsCursors(t *testing.T) {
	f := newFixture(t)
	e, err := f.pub.Build(context.Background(), 1, "", false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	cov := f.st.coverage[e.ID]
	if len(cov) != 2 {
		t.Fatalf("want 2 coverage leaves (asset 0xc has nothing scanned), got %d", len(cov))
	}
	// 0xa: backfill reached 4, so covered from 5; tail 25 is capped at the epoch top 20.
	if cov[0].Asset != common.HexToAddress("0xa") || cov[0].FromBlock != 5 || cov[0].ToBlock != 20 {
		t.Fatalf("asset 0xa coverage: %+v", cov[0])
	}
	// 0xb: backfill done at floor 12, tail 18.
	if cov[1].Asset != common.HexToAddress("0xb") || cov[1].FromBlock != 12 || cov[1].ToBlock != 18 {
		t.Fatalf("asset 0xb coverage: %+v", cov[1])
	}
	// The epoch widens to the lowest covered block, since claims must sit inside it.
	if e.FromBlock != 5 || e.ToBlock != 20 {
		t.Fatalf("epoch range: want [5,20], got [%d,%d]", e.FromBlock, e.ToBlock)
	}
	if e.CoverageRoot == (common.Hash{}) {
		t.Fatal("coverage root must be set")
	}
	if cov[0].RegistryKey != AssetKey(1, common.HexToAddress("0xa")) {
		t.Fatal("coverage key must be the registry's assetKey")
	}
}

func TestBuildRepostsWhenOnlyCoverageMoved(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e, _ := f.pub.Build(ctx, 1, "", false)
	if _, err := f.pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Same index, wider coverage: worth posting, because it is worth claiming.
	f.st.cursors[0].TailBlock = 40
	f.st.to = 40
	if _, err := f.pub.Build(ctx, 1, "", false); err != nil {
		t.Fatalf("build after coverage moved: %v", err)
	}
}

func TestBuildRefusesUnfundedCoverage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.pub.MinReward = big.NewInt(100)
	f.reg.quote = big.NewInt(10) // two leaves quote 20 in total

	if _, err := f.pub.Build(ctx, 1, "", false); !errors.Is(err, ErrUnfunded) {
		t.Fatalf("want ErrUnfunded, got %v", err)
	}
	if len(f.st.epochs) != 0 {
		t.Fatal("an unfunded build must not store a row")
	}

	f.reg.quote = big.NewInt(60)
	e, err := f.pub.Build(ctx, 1, "", false)
	if err != nil {
		t.Fatalf("funded build: %v", err)
	}
	if e.ExpectedRewardWei != "120" {
		t.Fatalf("expected reward should be recorded: %q", e.ExpectedRewardWei)
	}
}

func TestClaimDue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.reg.quote = big.NewInt(7)

	e, _ := f.pub.Build(ctx, 1, "", false)
	if _, err := f.pub.Publish(ctx, e.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = f.st.SetEpochStatus(ctx, e.ID, store.EpochFinalized)

	// Nothing claimable: marked claimed, no transaction.
	f.reg.quote = nil
	submits := f.sub.submits
	if n, err := f.pub.ClaimDue(ctx, 1); err != nil || n != 0 {
		t.Fatalf("claim with nothing claimable: n=%d err=%v", n, err)
	}
	if f.sub.submits != submits {
		t.Fatal("must not send a transaction when nothing is claimable")
	}
	if got, _ := f.st.GetEpoch(ctx, e.ID); got.ClaimedAt == nil || got.ClaimTx != nil || got.RewardWei != "0" {
		t.Fatalf("should be marked claimed for zero: %+v", got)
	}

	// A second epoch with something to claim sends one claimCoverage carrying the
	// CoverageRewarded logs, whose rewards are summed.
	f.reg.quote = big.NewInt(7)
	f.st.cursors[0].TailBlock = 30
	f.st.to = 30
	e2, err := f.pub.Build(ctx, 1, "", false)
	if err != nil {
		t.Fatalf("build 2: %v", err)
	}
	f.sub.wait = func(ref common.Hash) (*types.Receipt, error) { return publishedReceipt(f.reg, ref, 8), nil }
	if _, err := f.pub.Publish(ctx, e2.ID); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	_ = f.st.SetEpochStatus(ctx, e2.ID, store.EpochFinalized)

	ev := f.reg.abi.Events["CoverageRewarded"]
	rewardLog := func(reward int64) *types.Log {
		data, err := ev.Inputs.NonIndexed().Pack(uint64(5), uint64(30), uint64(26), big.NewInt(reward))
		if err != nil {
			t.Fatal(err)
		}
		return &types.Log{Address: f.reg.addr, Topics: []common.Hash{ev.ID, {}, {}, {}}, Data: data}
	}
	f.sub.wait = func(ref common.Hash) (*types.Receipt, error) {
		return &types.Receipt{Status: types.ReceiptStatusSuccessful, TxHash: ref,
			Logs: []*types.Log{rewardLog(100), rewardLog(23),
				// Another contract's log with the same topic is ignored.
				{Address: common.HexToAddress("0xdead"), Topics: []common.Hash{ev.ID}, Data: rewardLog(999).Data}}}, nil
	}
	submits = f.sub.submits
	n, err := f.pub.ClaimDue(ctx, 1)
	if err != nil || n != 1 {
		t.Fatalf("claim: n=%d err=%v", n, err)
	}
	if f.sub.submits != submits+1 || f.sub.calls[len(f.sub.calls)-1] != selector(t, f.reg.abi, "claimCoverage") {
		t.Fatal("want exactly one claimCoverage submission")
	}
	got, _ := f.st.GetEpoch(ctx, e2.ID)
	if got.ClaimTx == nil || got.RewardWei != "123" {
		t.Fatalf("claim should record the tx and the summed reward: %+v", got)
	}
	// Claimed epochs are not claimed again.
	if n, _ := f.pub.ClaimDue(ctx, 1); n != 0 {
		t.Fatal("must not claim twice")
	}
}
