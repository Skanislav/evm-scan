package api

import (
	"context"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// slowRegistry answers getFunding with a fixed tuple after a pause, and counts
// how often it was asked — the two things the cache exists to control.
type slowRegistry struct {
	calls atomic.Int32
	delay time.Duration
	gate  chan struct{} // when non-nil, a call waits here before answering
}

func (r *slowRegistry) CallAtHead(ctx context.Context, _ ethereum.CallMsg) ([]byte, error) {
	r.calls.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// (balance, paidFrom, paidTo, vouched), each one word.
	out := make([]byte, 128)
	big.NewInt(7).FillBytes(out[0:32])
	big.NewInt(100).FillBytes(out[32:64])
	big.NewInt(200).FillBytes(out[64:96])
	big.NewInt(9000).FillBytes(out[96:128])
	return out, nil
}

func newSlowRegistry(t *testing.T, r *slowRegistry) *hintreg.Client {
	t.Helper()
	c, err := hintreg.NewClient(r, common.HexToAddress("0x9999999999999999999999999999999999999999"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFundingListNeverWaitsOnTheChain(t *testing.T) {
	reg := &slowRegistry{gate: make(chan struct{})}
	client := newSlowRegistry(t, reg)
	c := newFundingCache()
	key := hintreg.AssetKey(1, common.HexToAddress("0x1"))

	// First ask: nothing known, and the answer is immediate even though the
	// registry has not answered yet.
	done := make(chan struct{})
	go func() { c.get(client, key); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("get blocked on the registry")
	}
	// The background read reaches the registry once, however many times the list
	// asks meanwhile.
	deadline := time.Now().Add(2 * time.Second)
	for reg.calls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no background read started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		if _, ok := c.get(client, key); ok {
			t.Fatal("reported a funding it cannot have read yet")
		}
	}
	if n := reg.calls.Load(); n != 1 {
		t.Fatalf("registry asked %d times while one read was in flight", n)
	}

	close(reg.gate)
	deadline = time.Now().Add(2 * time.Second)
	for {
		f, ok := c.get(client, key)
		if ok {
			if f.Vouched == nil || f.Vouched.Int64() != 9000 || f.Balance.Int64() != 7 {
				t.Fatalf("cached the wrong reading: %+v", f)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background read never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := reg.calls.Load(); n != 1 {
		t.Fatalf("a fresh reading was re-read: %d calls", n)
	}
}

func TestFundingStaleReadingIsServedThenRefreshed(t *testing.T) {
	reg := &slowRegistry{}
	client := newSlowRegistry(t, reg)
	c := newFundingCache()
	key := hintreg.AssetKey(1, common.HexToAddress("0x2"))

	now := time.Now()
	c.now = func() time.Time { return now }
	if _, err := c.fetch(context.Background(), client, key); err != nil {
		t.Fatal(err)
	}
	if n := reg.calls.Load(); n != 1 {
		t.Fatalf("fetch made %d calls", n)
	}

	// Past the TTL the old reading is still what a list gets — and a refresh starts.
	now = now.Add(fundingTTL + time.Second)
	f, ok := c.get(client, key)
	if !ok || f.Vouched.Int64() != 9000 {
		t.Fatalf("stale reading should still be served, got ok=%v %+v", ok, f)
	}
	deadline := time.Now().Add(2 * time.Second)
	for reg.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no refresh started for a stale reading")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFundingPoolIsBounded(t *testing.T) {
	reg := &slowRegistry{gate: make(chan struct{})}
	client := newSlowRegistry(t, reg)
	c := newFundingCache()

	for i := 0; i < fundingWorkers*3; i++ {
		c.get(client, hintreg.AssetKey(1, common.BigToAddress(big.NewInt(int64(i+1)))))
	}
	// Give the goroutines a moment to reach the registry.
	time.Sleep(50 * time.Millisecond)
	if n := reg.calls.Load(); int(n) != fundingWorkers {
		t.Fatalf("%d reads in flight, want %d", n, fundingWorkers)
	}
	close(reg.gate)
}
