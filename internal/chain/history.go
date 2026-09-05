package chain

import (
	"context"
	"fmt"
)

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
func HistoryFloor(ctx context.Context, src Source, head uint64) (uint64, error) {
	servable := func(n uint64) (bool, error) {
		_, err := src.Logs(ctx, Query{From: n, To: n})
		if err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}

	// If the head itself cannot be served, the node is not usable and a binary search
	// would silently return `head` as the floor, hiding the real problem.
	if ok, err := servable(head); err != nil {
		return 0, err
	} else if !ok {
		return 0, fmt.Errorf("chain: node cannot serve logs at head %d", head)
	}

	if ok, err := servable(0); err != nil {
		return 0, err
	} else if ok {
		return 0, nil
	}

	// Invariant: lo is known-unservable, hi is known-servable.
	lo, hi := uint64(0), head
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := servable(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, nil
}
