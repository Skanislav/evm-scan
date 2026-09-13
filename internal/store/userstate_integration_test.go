package store_test

import (
	"context"
	"errors"
	"flag"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Skanislav/evm-scan/internal/store"
	"github.com/Skanislav/evm-scan/internal/userstate"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var stateDSN = flag.String("state-dsn", "", "disposable PostgreSQL DSN for portable state tests")

func stateStore(t *testing.T) *store.Store {
	t.Helper()
	if *stateDSN != "" {
		ctx := context.Background()
		base, err := store.Open(ctx, *stateDSN)
		if err != nil {
			t.Fatal(err)
		}
		schema := "state_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if _, err := base.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
			base.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = base.Pool().Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`); base.Close() })
		u, err := url.Parse(*stateDSN)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		t.Setenv("EVMSCAN_TEST_DSN", u.String())
	}
	return open(t)
}

func TestUserStateAtomicRestoreAndLegacy(t *testing.T) {
	st := stateStore(t)
	ctx := context.Background()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	account := crypto.PubkeyToAddress(key.PublicKey)
	token := common.HexToAddress("0xa1")
	chain := uint64(11155111)
	sign := func(s userstate.Snapshot) userstate.Snapshot {
		t.Helper()
		tr, err := userstate.Build(s.Entries)
		if err != nil {
			t.Fatal(err)
		}
		s.StateRoot = tr.Root
		id, err := s.ID()
		if err != nil {
			t.Fatal(err)
		}
		s.Signature, err = crypto.Sign(id[:], key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := sign(userstate.Snapshot{Version: 1, Account: account, Revision: "1", Deadline: strconv.FormatInt(time.Now().Unix()+3600, 10), Entries: []userstate.Entry{{Kind: "verdict", ChainID: "11155111", Address: token, Weight: -1}, {Kind: "asset", ChainID: "8453", Address: token}}})
	if err := st.ImportState(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserState(ctx, account); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("import changed head", err)
	}
	before, err := st.DemandFor(ctx, chain, []common.Address{token})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutUserState(ctx, s, "0"); err != nil {
		t.Fatal(err)
	}
	view, err := st.UserState(ctx, account)
	if err != nil || view.Generation != "1" || view.LegacyChanged {
		t.Fatalf("view %+v %v", view, err)
	}
	after, err := st.DemandFor(ctx, chain, []common.Address{token})
	if err != nil {
		t.Fatal(err)
	}
	if after[token].Against != before[token].Against+1 {
		t.Fatalf("demand not projected: %v %v", before, after)
	}
	if err := st.PutUserState(ctx, s, "0"); err != nil {
		t.Fatal("idempotent retry", err)
	}
	id, _, _ := s.Validate()
	next := s
	next.Revision = "2"
	next.Previous = id
	next.Entries = nil
	next = sign(next)
	if _, _, err := st.ReplaceVerdicts(ctx, chain, account, big.NewInt(time.Now().Unix()+7200), []store.Verdict{{Address: token, Weight: 1}}); err != nil {
		t.Fatal(err)
	}
	changed, err := st.UserState(ctx, account)
	if err != nil || !changed.LegacyChanged || changed.Generation != "2" {
		t.Fatalf("legacy divergence %+v %v", changed, err)
	}
	if err := st.PutUserState(ctx, next, "1"); !errors.Is(err, store.ErrStateConflict) {
		t.Fatal("accepted concurrent stale projection", err)
	}
	if err := st.PutUserState(ctx, next, "2"); err != nil {
		t.Fatal(err)
	}
	totals, _ := st.DemandFor(ctx, chain, []common.Address{token})
	if totals[token] != before[token] {
		t.Fatalf("clear did not retract: %v != %v", totals, before)
	}
	assets, err := st.AssetCommit(ctx, account)
	if err != nil || len(assets.Items) != 0 {
		t.Fatal("empty asset projection", err)
	}
	if err := st.PutUserState(ctx, s, "3"); !errors.Is(err, store.ErrStateConflict) {
		t.Fatal("accepted replay", err)
	}
	c, err := st.BuildStateCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	id2, _, _ := next.Validate()
	a := next
	a.Revision = "3"
	a.Previous = id2
	a.Entries = []userstate.Entry{{Kind: "asset", ChainID: "1", Address: token}}
	a = sign(a)
	b := next
	b.Revision = "3"
	b.Previous = id2
	b.Entries = []userstate.Entry{{Kind: "asset", ChainID: "2", Address: token}}
	b = sign(b)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, v := range []userstate.Snapshot{a, b} {
		wg.Add(1)
		go func(v userstate.Snapshot) { defer wg.Done(); results <- st.PutUserState(ctx, v, "3") }(v)
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, store.ErrStateConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("race accepted %d conflicts %d", ok, conflict)
	}
}

// A replacement database reconstructs the commitment from the portable export,
// without promoting accounts, recreating demand, or needing the source's salt.
func TestUserStateExportToFreshDatabase(t *testing.T) {
	st := stateStore(t)
	ctx := context.Background()
	c, err := st.BuildStateCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Make the fixture independent of preceding tests and the original voter salt.
	key, _ := crypto.GenerateKey()
	v := userstate.Snapshot{Version: 1, Account: crypto.PubkeyToAddress(key.PublicKey), Revision: "1", Deadline: "100", Entries: []userstate.Entry{{Kind: "verdict", ChainID: "1", Address: common.HexToAddress("0xab"), Weight: -1}}}
	tr, _ := userstate.Build(v.Entries)
	v.StateRoot = tr.Root
	id, _ := v.ID()
	v.Signature, _ = crypto.Sign(id[:], key)
	c = userstate.Checkpoint{Version: 1, Accounts: []userstate.AccountRevision{{Account: v.Account, ID: id}}, Snapshots: []userstate.Snapshot{v}}
	agg, _ := userstate.Aggregate(c.Accounts)
	c.Root = agg.Root
	schema := "state_restore_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := st.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer st.Pool().Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	dsn := os.Getenv("EVMSCAN_TEST_DSN")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	fresh, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fresh.ImportCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.UserState(ctx, v.Account); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restored history became live", err)
	}
	restored, err := fresh.StateCheckpoint(ctx, c.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatal(err)
	}
	if restored.Root != c.Root {
		t.Fatal("restore changed root")
	}
	var demands int
	if err := fresh.Pool().QueryRow(ctx, `SELECT count(*) FROM asset_demand`).Scan(&demands); err != nil || demands != 0 {
		t.Fatal("import created demand", err)
	}
	next := v
	next.Revision = "2"
	next.Previous = id
	next.Deadline = strconv.FormatInt(time.Now().Unix()+3600, 10)
	nextID, _ := next.ID()
	next.Signature, _ = crypto.Sign(nextID[:], key)
	if err := fresh.PutUserState(ctx, next, "0"); err != nil {
		t.Fatal("cannot continue from imported history", err)
	}
	totals, err := fresh.DemandFor(ctx, 1, []common.Address{v.Entries[0].Address})
	if err != nil || totals[v.Entries[0].Address].Against != 1 {
		t.Fatal("fresh signature did not project", err)
	}
}
