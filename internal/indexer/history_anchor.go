package indexer

import (
	"context"

	"github.com/Skanislav/evm-scan/internal/chain"
)

// historyAnchor picks a block this node is known to have served logs for, so the
// floor probe can catch a node that answers pruned blocks with an empty log set
// instead of an error (see chain.ProbeHistoryFloor and chain.ErrEmptyHistory).
//
// Discovery's oldest candidate row is that block: it was written because an
// eth_getLogs over the block returned a watched event, so an unfiltered query over
// the same block must return at least one log. On a first start the table is empty
// and the probe runs without the cross-check; the anchor grows older, and the check
// stronger, the longer the daemon has been watching.
func (s *Service) historyAnchor(ctx context.Context) *chain.Anchor {
	block, ok, err := s.st.OldestCandidateBlock(ctx, s.chainID)
	if err != nil {
		s.log.Debug("history anchor lookup failed; probing without cross-check", "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &chain.Anchor{Block: block, MinLogs: 1}
}
