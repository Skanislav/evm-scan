package chain

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// errTimeout is what a JSON-RPC round trip yields when the socket read times out.
var errTimeout = &net.OpError{Op: "read", Net: "unix", Err: errors.New("i/o timeout")}

// fastRetry keeps the retry path exercised without making the tests wait.
var fastRetry = RetryPolicy{Attempts: 3, Backoff: time.Microsecond, MaxBackoff: time.Microsecond}

// prunedNode serves logs only at or above floor, which is how a node that dropped
// ancient receipts behaves.
type prunedNode struct {
	floor uint64
	// probes counts Logs calls, so the search's cost can be asserted.
	probes int
	// deadAtHead makes even the head unservable, modelling a broken node.
	deadAtHead bool
	head       uint64

	// prunedErr is the error returned for a pruned block; nil means geth's
	// "receipt not found".
	prunedErr error
	// emptyForPruned makes pruned blocks answer with an empty log set and no error,
	// which is how some non-geth clients behave.
	emptyForPruned bool
	// logsAt is how many logs a servable block returns; blocks not listed return none.
	logsAt map[uint64]int
	// failOnce queues errors to return, one per call, before a block answers normally.
	failOnce map[uint64][]error
	// failAlways makes a block return the same error on every call.
	failAlways map[uint64]error
	// flaky makes the first call for every block fail with a timeout.
	flaky bool
	seen  map[uint64]bool
}

func (p *prunedNode) Logs(_ context.Context, q Query) ([]types.Log, error) {
	p.probes++
	if p.deadAtHead && q.From == p.head {
		return nil, errors.New("no backend")
	}
	if errs := p.failOnce[q.From]; len(errs) > 0 {
		p.failOnce[q.From] = errs[1:]
		return nil, errs[0]
	}
	if err := p.failAlways[q.From]; err != nil {
		return nil, err
	}
	if p.flaky {
		if p.seen == nil {
			p.seen = map[uint64]bool{}
		}
		if !p.seen[q.From] {
			p.seen[q.From] = true
			return nil, errTimeout
		}
	}
	if q.From < p.floor {
		if p.emptyForPruned {
			return nil, nil
		}
		if p.prunedErr != nil {
			return nil, p.prunedErr
		}
		return nil, errors.New("receipt not found")
	}
	return make([]types.Log, p.logsAt[q.From]), nil
}

func (p *prunedNode) ChainID(context.Context) (uint64, error)   { return 1, nil }
func (p *prunedNode) HeadBlock(context.Context) (uint64, error) { return p.head, nil }
func (p *prunedNode) HeaderHash(context.Context, uint64) (common.Hash, error) {
	return common.Hash{}, nil
}
func (p *prunedNode) SubscribeLogs(context.Context, Query, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, ErrNotStreaming
}
func (p *prunedNode) CallAtHead(context.Context, ethereum.CallMsg) ([]byte, error) { return nil, nil }
func (p *prunedNode) CodeAt(context.Context, common.Address) ([]byte, error)       { return nil, nil }
func (p *prunedNode) NonceAt(context.Context, common.Address) (uint64, error)      { return 0, nil }
func (p *prunedNode) Endpoint() Endpoint                                           { return Endpoint{} }
func (p *prunedNode) Close()                                                       {}

func TestHistoryFloorFindsThePruningBoundary(t *testing.T) {
	const head = 1_000_000
	for _, floor := range []uint64{1, 2, 4999, 500_000, 999_999} {
		node := &prunedNode{floor: floor, head: head}

		got, err := HistoryFloor(context.Background(), node, head)
		if err != nil {
			t.Fatalf("floor %d: %v", floor, err)
		}
		if got != floor {
			t.Errorf("floor %d: got %d", floor, got)
		}
	}
}

// TestHistoryFloorReportsZeroForFullHistory is the case that must stay cheap: a node
// with everything should be settled by the single probe at block 0.
func TestHistoryFloorReportsZeroForFullHistory(t *testing.T) {
	node := &prunedNode{floor: 0, head: 1_000_000}

	got, err := HistoryFloor(context.Background(), node, node.head)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("floor = %d, want 0", got)
	}
	if node.probes > 2 {
		t.Errorf("took %d probes for a full-history node, want at most 2", node.probes)
	}
}

// TestHistoryFloorIsLogarithmic guards against the search degrading into a linear
// walk, which on a mainnet-sized chain would be millions of RPC calls.
func TestHistoryFloorIsLogarithmic(t *testing.T) {
	node := &prunedNode{floor: 12_345_678, head: 21_000_000}

	if _, err := HistoryFloor(context.Background(), node, node.head); err != nil {
		t.Fatal(err)
	}
	if node.probes > 30 {
		t.Errorf("took %d probes, want O(log head)", node.probes)
	}
}

// TestHistoryFloorRejectsUnusableNode: without this check the binary search would
// return `head` and the caller would conclude it simply has no history, hiding a
// node that is actually broken.
func TestHistoryFloorRejectsUnusableNode(t *testing.T) {
	node := &prunedNode{floor: 0, head: 500, deadAtHead: true}

	if _, err := HistoryFloor(context.Background(), node, node.head); err == nil {
		t.Error("expected an error when the node cannot serve logs at the head")
	}
}

func TestHistoryFloorHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	node := &prunedNode{floor: 100, head: 1000}
	if _, err := HistoryFloor(ctx, node, node.head); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestHistoryFloorRetriesATransientError: a timeout on the very first midpoint of the
// search used to be read as "pruned", which would have put the floor at 500_000
// instead of 250_000. It must be retried and leave the answer untouched.
func TestHistoryFloorRetriesATransientError(t *testing.T) {
	const head, floor = 1_000_000, 250_000
	node := &prunedNode{
		floor:    floor,
		head:     head,
		failOnce: map[uint64][]error{500_000: {errTimeout}},
	}

	res, err := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{Retry: fastRetry})
	if err != nil {
		t.Fatal(err)
	}
	if res.Floor != floor {
		t.Errorf("floor = %d, want %d: the transient error moved it", res.Floor, floor)
	}
	if res.Boundary != LogsErrHistoryUnavailable {
		t.Errorf("boundary = %s, want history_unavailable", res.Boundary)
	}
}

// TestHistoryFloorSurvivesAFlakyNode makes every block fail once with a timeout. The
// floor must still come out exact, and the cost must stay two calls per probe.
func TestHistoryFloorSurvivesAFlakyNode(t *testing.T) {
	const head, floor = 1_000_000, 4999
	node := &prunedNode{floor: floor, head: head, flaky: true}

	res, err := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{Retry: fastRetry})
	if err != nil {
		t.Fatal(err)
	}
	if res.Floor != floor {
		t.Errorf("floor = %d, want %d", res.Floor, floor)
	}
	if res.Probes > 2*30 {
		t.Errorf("took %d probes, want at most two per binary-search step", res.Probes)
	}
}

// TestHistoryFloorGivesUpOnPersistentTransientFailure: when a block never answers,
// the probe must say so rather than hand back a floor it could not verify.
func TestHistoryFloorGivesUpOnPersistentTransientFailure(t *testing.T) {
	const head = 1_000_000
	for name, err := range map[string]error{
		"net timeout":    errTimeout,
		"econnreset":     &net.OpError{Op: "read", Err: syscall.ECONNRESET},
		"rate limited":   errors.New("429 Too Many Requests"),
		"server timeout": codedErr{code: -32002, msg: "request timed out"},
	} {
		t.Run(name, func(t *testing.T) {
			node := &prunedNode{
				floor:      100,
				head:       head,
				failAlways: map[uint64]error{500_000: err},
			}
			_, perr := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{Retry: fastRetry})
			if !errors.Is(perr, ErrProbeExhausted) {
				t.Fatalf("err = %v, want ErrProbeExhausted", perr)
			}
			if _, ferr := HistoryFloor(context.Background(), &prunedNode{floor: 100, head: head}, head); ferr != nil {
				t.Fatalf("control run without failures errored: %v", ferr)
			}
		})
	}
}

// TestHistoryFloorCatchesEmptyPrunedHistory models a client that answers pruned
// blocks with an empty result. Without an anchor the probe cannot tell and reports
// 0; with one it must refuse.
func TestHistoryFloorCatchesEmptyPrunedHistory(t *testing.T) {
	const head, realFloor = 100_000, 5000
	mk := func() *prunedNode {
		return &prunedNode{
			floor:          realFloor,
			head:           head,
			emptyForPruned: true,
			logsAt:         map[uint64]int{90_000: 3},
		}
	}

	// The blind probe is fooled — this is the failure mode the anchor exists for.
	if got, err := HistoryFloor(context.Background(), mk(), head); err != nil || got != 0 {
		t.Fatalf("blind probe: floor=%d err=%v, expected the fooled answer 0", got, err)
	}

	// An anchor inside the pruned region exposes it.
	res, err := ProbeHistoryFloor(context.Background(), mk(), head, HistoryFloorOptions{
		Anchor: &Anchor{Block: 1000},
		Retry:  fastRetry,
	})
	if !errors.Is(err, ErrEmptyHistory) {
		t.Fatalf("err = %v, want ErrEmptyHistory", err)
	}
	if !res.AnchorChecked {
		t.Error("anchor was not checked")
	}
}

// TestHistoryFloorAnchorPassesOnAnHonestNode: the cross-check must not get in the
// way of a node that behaves — and must not waste a call on an anchor the node
// already admits it cannot serve.
func TestHistoryFloorAnchorPassesOnAnHonestNode(t *testing.T) {
	const head, floor = 100_000, 5000
	node := &prunedNode{floor: floor, head: head, logsAt: map[uint64]int{90_000: 3}}

	res, err := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{
		Anchor: &Anchor{Block: 90_000, MinLogs: 3},
		Retry:  fastRetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Floor != floor || !res.AnchorChecked {
		t.Errorf("floor=%d checked=%v, want %d and true", res.Floor, res.AnchorChecked, floor)
	}

	// Asking for more logs than the block has is the same signal as an empty answer.
	if _, err := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{
		Anchor: &Anchor{Block: 90_000, MinLogs: 4},
		Retry:  fastRetry,
	}); !errors.Is(err, ErrEmptyHistory) {
		t.Errorf("short anchor: err = %v, want ErrEmptyHistory", err)
	}

	// An anchor below the floor is not the node's claim to check.
	before := node.probes
	res, err = ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{
		Anchor: &Anchor{Block: 10},
		Retry:  fastRetry,
	})
	if err != nil || res.AnchorChecked {
		t.Errorf("anchor below floor: err=%v checked=%v, want nil and false", err, res.AnchorChecked)
	}
	if res.Probes != node.probes-before {
		t.Errorf("probe accounting: res.Probes=%d, node saw %d", res.Probes, node.probes-before)
	}
}

// TestHistoryFloorReportsAnUnrecognisedBoundary: an error the classifier has never
// seen still marks the block pruned (the safe reading for a backfill floor), but the
// result must say the floor rests on a guess and carry the evidence.
func TestHistoryFloorReportsAnUnrecognisedBoundary(t *testing.T) {
	const head, floor = 1_000_000, 777
	odd := errors.New("some client-specific wording nobody has catalogued")
	node := &prunedNode{floor: floor, head: head, prunedErr: odd}

	res, err := ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{Retry: fastRetry})
	if err != nil {
		t.Fatal(err)
	}
	if res.Floor != floor {
		t.Errorf("floor = %d, want %d", res.Floor, floor)
	}
	if res.Boundary != LogsErrUnknown || !errors.Is(res.BoundaryErr, odd) {
		t.Errorf("boundary = %s / %v, want unknown carrying the original error", res.Boundary, res.BoundaryErr)
	}

	// geth's typed answer is recognised as definite.
	node = &prunedNode{floor: floor, head: head, prunedErr: codedErr{code: 4444, msg: "pruned history unavailable"}}
	res, err = ProbeHistoryFloor(context.Background(), node, head, HistoryFloorOptions{Retry: fastRetry})
	if err != nil {
		t.Fatal(err)
	}
	if res.Boundary != LogsErrHistoryUnavailable {
		t.Errorf("boundary = %s, want history_unavailable", res.Boundary)
	}
}

// codedErr is what the rpc client hands back for a JSON-RPC error object.
type codedErr struct {
	code int
	msg  string
}

func (e codedErr) Error() string  { return e.msg }
func (e codedErr) ErrorCode() int { return e.code }

var _ rpc.Error = codedErr{}

func TestClassifyLogsError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want LogsErrorKind
	}{
		{"geth pruned history code", codedErr{code: 4444, msg: "pruned history unavailable"}, LogsErrHistoryUnavailable},
		{"geth pruned history text", errors.New("pruned history unavailable"), LogsErrHistoryUnavailable},
		{"geth missing receipts", errors.New("failed to get logs for block #4290 (0xabcdef)"), LogsErrHistoryUnavailable},
		{"geth header not found", errors.New("header not found"), LogsErrHistoryUnavailable},
		{"geth unknown block", errors.New("unknown block"), LogsErrHistoryUnavailable},
		{"geth receipt not found", errors.New("receipt not found"), LogsErrHistoryUnavailable},
		{"geth missing trie node", errors.New("missing trie node 0xdead (path ) state 0xbeef is not available"), LogsErrHistoryUnavailable},
		{"other client history", errors.New("history not available for block 12"), LogsErrHistoryUnavailable},
		{"definite wins over transient wording", errors.New("pruned history unavailable (request timed out)"), LogsErrHistoryUnavailable},

		{"context deadline", context.DeadlineExceeded, LogsErrTransient},
		{"context canceled wrapped", errors.Join(errors.New("getLogs"), context.Canceled), LogsErrTransient},
		{"net timeout", errTimeout, LogsErrTransient},
		{"econnreset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, LogsErrTransient},
		{"econnrefused", syscall.ECONNREFUSED, LogsErrTransient},
		{"eof", errors.New("EOF"), LogsErrTransient},
		{"geth server timeout code", codedErr{code: -32002, msg: "request timed out"}, LogsErrTransient},
		{"provider limit code", codedErr{code: -32005, msg: "limit exceeded"}, LogsErrTransient},
		{"http 429", rpc.HTTPError{StatusCode: 429, Status: "429 Too Many Requests"}, LogsErrTransient},
		{"http 503", rpc.HTTPError{StatusCode: 503, Status: "503 Service Unavailable"}, LogsErrTransient},
		{"busy text", errors.New("server is busy, try again later"), LogsErrTransient},
		{"client closed", rpc.ErrClientQuit, LogsErrTransient},

		// A bare "not found" is never evidence about a block: these are a proxy in
		// front of the RPC and a node that does not serve the method. Reading either
		// as pruned history would set the backfill floor from a misconfiguration.
		{"http 404 from a proxy", rpc.HTTPError{StatusCode: 404, Status: "404 Not Found",
			Body: []byte("404 Not Found")}, LogsErrTransient},
		{"method not found", codedErr{code: -32601, msg: "the method eth_getLogs does not exist/is not available"}, LogsErrUnknown},
		{"invalid request", codedErr{code: -32600, msg: "invalid request"}, LogsErrUnknown},
		{"bare not found text", errors.New("not found"), LogsErrUnknown},

		{"nothing recognisable", errors.New("no backend"), LogsErrUnknown},
		{"nil", nil, LogsErrUnknown},
	}
	for _, c := range cases {
		if got := ClassifyLogsError(c.err); got != c.want {
			t.Errorf("%s: %v -> %s, want %s", c.name, c.err, got, c.want)
		}
	}
}
