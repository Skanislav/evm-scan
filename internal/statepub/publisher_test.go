package statepub

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/userstate"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var pubDSN = flag.String("state-pub-dsn", "", "disposable Postgres for state publisher tests")

type fakeNode struct {
	chain.Source
	chain.Sender
	chain       uint64
	head        uint64
	canonical   common.Hash
	receipt     *types.Receipt
	record      string
	sent        []*types.Transaction
	gas         uint64
	fee         *big.Int
	estimateErr error
}

func (n *fakeNode) ChainID(context.Context) (uint64, error)                        { return n.chain, nil }
func (n *fakeNode) HeadBlock(context.Context) (uint64, error)                      { return n.head, nil }
func (n *fakeNode) HeaderHash(context.Context, uint64) (common.Hash, error)        { return n.canonical, nil }
func (n *fakeNode) PendingNonceAt(context.Context, common.Address) (uint64, error) { return 0, nil }
func (n *fakeNode) SuggestGasPrice(context.Context) (*big.Int, error)              { return n.fee, nil }
func (n *fakeNode) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return n.gas, n.estimateErr
}
func (n *fakeNode) CallAtHead(context.Context, ethereum.CallMsg) ([]byte, error) {
	return ResolverABI.Methods["text"].Outputs.Pack(n.record)
}
func (n *fakeNode) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	if n.receipt == nil {
		return nil, ethereum.NotFound
	}
	return n.receipt, nil
}
func (n *fakeNode) SendTransaction(_ context.Context, tx *types.Transaction) error {
	n.sent = append(n.sent, tx)
	return nil
}
func TestPublisherRestartCapsAndReadback(t *testing.T) {
	if *pubDSN == "" {
		t.Skip("set -state-pub-dsn to a disposable database")
	}
	ctx := context.Background()
	base, err := store.Open(ctx, *pubDSN)
	if err != nil {
		t.Fatal(err)
	}
	schema := "state_pub_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := base.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	defer func() { _, _ = base.Pool().Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); base.Close() }()
	u, err := url.Parse(*pubDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../userstate/testdata/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Snapshot userstate.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if err := st.PutUserState(ctx, f.Snapshot, "0"); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.GenerateKey()
	n := &fakeNode{chain: ChainID, gas: 100000, fee: big.NewInt(10), head: 30, canonical: common.HexToHash("0xaa")}
	p := &Publisher{Store: st, Node: n, Key: key, Resolver: common.HexToAddress("0x1234"), Namehash: common.HexToHash("0x5678"), MaxGas: 200000, MaxFee: big.NewInt(20), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.chain = 1
	if err := p.Tick(ctx, true); err == nil {
		t.Fatal("accepted wrong chain")
	}
	n.chain = ChainID
	n.estimateErr = errors.New("no ENS permission")
	if err := p.Tick(ctx, true); err == nil {
		t.Fatal("ignored failed estimate")
	}
	n.estimateErr = nil
	n.fee = big.NewInt(21)
	if err := p.Tick(ctx, true); err == nil {
		t.Fatal("ignored fee cap")
	}
	n.fee = big.NewInt(10)
	n.gas = 200001
	if err := p.Tick(ctx, true); err == nil {
		t.Fatal("ignored gas cap")
	}
	n.gas = 100000
	if err := p.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.StatePublications(ctx)
	if len(jobs) != 1 || len(jobs[0].Raw) == 0 || len(n.sent) != 1 {
		t.Fatalf("not durably submitted %+v", jobs)
	}
	// A replacement publisher resumes the persisted signed bytes, never resigns.
	restarted := *p
	if err := restarted.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 2 || n.sent[0].Hash() != n.sent[1].Hash() {
		t.Fatal("restart changed transaction")
	}
	n.receipt = &types.Receipt{Status: 1, BlockNumber: big.NewInt(20), BlockHash: n.canonical}
	n.record = userstate.Record("states", jobs[0].Root)
	if err := p.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.StatePublications(ctx)
	if jobs[0].Status != "pending" {
		t.Fatal("confirmed shallow receipt")
	}
	n.head = 32
	if err := p.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.StatePublications(ctx)
	if jobs[0].Status != "confirmed" {
		t.Fatal("not confirmed", jobs[0])
	}
	if err := p.Tick(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 2 {
		t.Fatal("republished unchanged root")
	}
	n.receipt = nil
	if err := p.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.StatePublications(ctx)
	if jobs[0].Status != "pending" {
		t.Fatal("lost confirmation not rolled back")
	}
	n.receipt = &types.Receipt{Status: 0, BlockNumber: big.NewInt(20), BlockHash: n.canonical}
	if err := p.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ = st.StatePublications(ctx)
	if jobs[0].Status != "failed" {
		t.Fatal("revert not persisted")
	}
}
func TestResolverRecordRead(t *testing.T) {
	n := &fakeNode{record: "hello"}
	v, err := ReadRecord(context.Background(), n, common.Address{}, common.Hash{}, RecordKey)
	if err != nil || v != "hello" {
		t.Fatal(v, err)
	}
}
