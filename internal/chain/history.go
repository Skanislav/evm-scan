package chain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrEmptyHistory is returned when the node answers eth_getLogs for a block that is
// known to contain logs with an empty result. Such a node is reporting pruned blocks
// as empty rather than as errors, so a floor found by probing it cannot be trusted:
// the backfiller would believe it read history it never saw.
var ErrEmptyHistory = errors.New("chain: node returns empty logs for a block known to have them; history floor cannot be trusted")

// ErrProbeExhausted is returned when a probe keeps failing transiently and the
// retry budget runs out. The floor is unknown, not raised.
var ErrProbeExhausted = errors.New("chain: history probe exhausted retries on a transient error")

// Anchor is a block the caller knows to contain at least MinLogs logs — the registry's
// deploy block, or a block the discovery sweep already read. The probe uses it to
// catch a node that answers pruned blocks with an empty log set instead of an error.
type Anchor struct {
	Block uint64
	// MinLogs is the smallest log count that proves the block was really read.
	// Zero means 1.
	MinLogs int
}

// RetryPolicy bounds how the probe retries a transient eth_getLogs failure.
type RetryPolicy struct {
	// Attempts is the total number of tries per block, including the first.
	Attempts int
	// Backoff is the wait after the first failure; it doubles up to MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration
}

func (r RetryPolicy) withDefaults() RetryPolicy {
	if r.Attempts <= 0 {
		r.Attempts = 5
	}
	if r.Backoff <= 0 {
		r.Backoff = 250 * time.Millisecond
	}
	if r.MaxBackoff <= 0 {
		r.MaxBackoff = 4 * time.Second
	}
	return r
}

// HistoryFloorOptions tunes ProbeHistoryFloor. The zero value is what HistoryFloor uses.
type HistoryFloorOptions struct {
	// Anchor, when set, is cross-checked after the search: if the node claims to
	// serve the anchor's block but returns fewer than MinLogs logs for it, the probe
	// fails with ErrEmptyHistory instead of returning a floor.
	Anchor *Anchor
	Retry  RetryPolicy
}

// HistoryProbe is the outcome of ProbeHistoryFloor.
type HistoryProbe struct {
	// Floor is the oldest block the node served logs for.
	Floor uint64
	// Boundary is how the block just below Floor was found to be unservable.
	// LogsErrHistoryUnavailable means the node said so in as many words;
	// LogsErrUnknown means the probe inferred it from an error it did not recognise,
	// and BoundaryErr carries that error so a caller can log or surface it.
	// For a full-history node (Floor == 0) Boundary is LogsErrUnknown and BoundaryErr nil.
	Boundary    LogsErrorKind
	BoundaryErr error
	// Probes is the number of eth_getLogs calls made, retries included.
	Probes int
	// AnchorChecked reports whether the anchor cross-check ran.
	AnchorChecked bool
}

// HistoryFloor finds the oldest block whose logs this node can still serve.
//
// There is no standard RPC for "how far back does your history go", so this binary
// searches eth_getLogs over single blocks. The query carries no address or topic
// filter deliberately: a filter that matches nothing can be answered from the header
// bloom alone, which would make a pruned block look available. An unfiltered query
// forces the node to actually read that block's receipts, which is the thing we are
// testing for.
//
// Availability is assumed monotone — every block at or above the floor is servable —
// which is how geth's history pruning behaves.
//
// The result is a floor for backfills. A node that kept everything reports 0; a node
// synced without ancient history reports wherever its receipts begin. Either way the
// indexer stops there instead of assuming genesis is reachable.
//
// This is ProbeHistoryFloor with default options: transient errors are retried, but
// there is no anchor, so a node that answers pruned blocks with an empty result is
// not detected. Callers that know a block with logs should use ProbeHistoryFloor.
func HistoryFloor(ctx context.Context, src Source, head uint64) (uint64, error) {
	p, err := ProbeHistoryFloor(ctx, src, head, HistoryFloorOptions{})
	if err != nil {
		return 0, err
	}
	return p.Floor, nil
}

// ProbeHistoryFloor is HistoryFloor with error classification, bounded retries and an
// optional anchor cross-check.
//
// Each single-block eth_getLogs is classified with ClassifyLogsError. A transient
// failure is retried with backoff and never counts as evidence; if the retries run
// out the probe returns ErrProbeExhausted rather than a floor that is wrong. A
// definite "history unavailable" answer marks the block pruned. An error the
// classifier does not recognise is also read as pruned — the conservative choice for
// a backfill floor — but the result records it in Boundary/BoundaryErr so the caller
// can see the floor rests on a guess.
//
// The anchor closes the other gap. Some clients return an empty log set, not an
// error, for a block whose receipts they dropped; on such a node every probe
// "succeeds" and the search reports 0. If the caller supplies a block known to hold
// logs and the node claims to serve it (anchor >= floor) yet returns fewer than
// MinLogs logs for it, the probe returns ErrEmptyHistory. An anchor below the
// found floor is not queried: the node already admits it has nothing there.
func ProbeHistoryFloor(ctx context.Context, src Source, head uint64, opts HistoryFloorOptions) (HistoryProbe, error) {
	opts.Retry = opts.Retry.withDefaults()
	var res HistoryProbe

	// probe answers "does the node serve logs for n" with retries on transient
	// failures. On !ok, kind and cause say why.
	probe := func(n uint64) (logs int, ok bool, kind LogsErrorKind, cause error, err error) {
		backoff := opts.Retry.Backoff
		for attempt := 1; ; attempt++ {
			if err := ctx.Err(); err != nil {
				return 0, false, LogsErrTransient, nil, err
			}
			res.Probes++
			got, qerr := src.Logs(ctx, Query{From: n, To: n})
			if qerr == nil {
				return len(got), true, LogsErrUnknown, nil, nil
			}
			if err := ctx.Err(); err != nil {
				return 0, false, LogsErrTransient, qerr, err
			}
			kind := ClassifyLogsError(qerr)
			if kind != LogsErrTransient {
				return 0, false, kind, qerr, nil
			}
			if attempt >= opts.Retry.Attempts {
				return 0, false, kind, qerr, fmt.Errorf("%w: block %d after %d attempts: %v",
					ErrProbeExhausted, n, attempt, qerr)
			}
			if err := sleep(ctx, backoff); err != nil {
				return 0, false, LogsErrTransient, qerr, err
			}
			if backoff *= 2; backoff > opts.Retry.MaxBackoff {
				backoff = opts.Retry.MaxBackoff
			}
		}
	}

	// If the head itself cannot be served, the node is not usable and a binary search
	// would silently return `head` as the floor, hiding the real problem.
	if _, ok, kind, cause, err := probe(head); err != nil {
		return res, err
	} else if !ok {
		return res, fmt.Errorf("chain: node cannot serve logs at head %d (%s): %w", head, kind, cause)
	}

	if _, ok, kind, cause, err := probe(0); err != nil {
		return res, err
	} else if ok {
		res.Floor = 0
	} else {
		// Invariant: lo is known-unservable, hi is known-servable.
		lo, hi := uint64(0), head
		res.Boundary, res.BoundaryErr = kind, cause
		for hi-lo > 1 {
			mid := lo + (hi-lo)/2
			_, ok, kind, cause, err := probe(mid)
			if err != nil {
				return res, err
			}
			if ok {
				hi = mid
			} else {
				lo = mid
				res.Boundary, res.BoundaryErr = kind, cause
			}
		}
		res.Floor = hi
	}

	if a := opts.Anchor; a != nil && a.Block <= head && a.Block >= res.Floor {
		want := a.MinLogs
		if want <= 0 {
			want = 1
		}
		res.AnchorChecked = true
		n, ok, kind, cause, err := probe(a.Block)
		if err != nil {
			return res, err
		}
		if !ok {
			// The node served a block below this one but not this one: availability
			// is not monotone here, and the floor the search found means nothing.
			return res, fmt.Errorf("chain: node serves block %d but not anchor block %d (%s): %w",
				res.Floor, a.Block, kind, cause)
		}
		if n < want {
			return res, fmt.Errorf("%w (anchor block %d: want >= %d logs, got %d; probed floor %d)",
				ErrEmptyHistory, a.Block, want, n, res.Floor)
		}
	}
	return res, nil
}

// sleep waits for d or until ctx is done, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
