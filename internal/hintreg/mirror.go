package hintreg

import (
	"context"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Skanislav/evm-scan/internal/store"
)

// HeadFunc reports the current head of an indexed chain. ok is false for a chain this
// deployment does not index, whose hints are then ignored.
type HeadFunc func(ctx context.Context, chainID uint64) (head uint64, ok bool, err error)

// CodeFunc reports the bytecode at an address on a chain we index. The mirror uses
// it to tell a real registration from one aimed at the wrong chain.
type CodeFunc func(ctx context.Context, chainID uint64, addr common.Address) ([]byte, error)

// Nudger lets the mirror wake a chain's follower after new registrations.
type Nudger interface{ Nudge() }

// NudgeFunc finds a chain's follower. It is a lookup rather than a prebuilt map
// because the set of running chains changes while the mirror is running: a chain
// added at 11am has to be nudged at 11am, not at the next restart. ok is false for
// a chain this deployment does not run.
type NudgeFunc func(chainID uint64) (Nudger, bool)

// Mirror pulls permissionlessly registered hints into the local scan set.
//
// Registration is open by design: anyone can point the indexer at any contract. That
// is safe precisely because a hint confers nothing — it only asks us to read public
// logs. Everything downstream treats registrant-supplied fields as untrusted.
type Mirror struct {
	client  *Client
	st      *store.Store
	head    HeadFunc
	code    CodeFunc
	nudge   NudgeFunc
	log     *slog.Logger
	regChID uint64
	// demandUnsupported is set once a registry without listDemand has been logged.
	demandUnsupported bool
}

// NewMirror builds a Mirror. regChainID is the chain the registry is deployed on,
// which need not be a chain we index.
func NewMirror(c *Client, st *store.Store, regChainID uint64, head HeadFunc, code CodeFunc, nudge NudgeFunc, log *slog.Logger) *Mirror {
	return &Mirror{
		client:  c,
		st:      st,
		head:    head,
		code:    code,
		nudge:   nudge,
		log:     log.With("registry", c.Address().Hex()),
		regChID: regChainID,
	}
}

// Run syncs on an interval until ctx is cancelled.
func (m *Mirror) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		if err := m.Sync(ctx); err != nil && ctx.Err() == nil {
			m.log.Error("registry sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Sync reconciles the registry's asset set into the store.
func (m *Mirror) Sync(ctx context.Context) error {
	assets, err := m.client.ListAssets(ctx, 200)
	if err != nil {
		return err
	}

	touched := map[uint64]bool{}
	for _, a := range assets {
		head, ok, err := m.head(ctx, a.ChainID)
		if err != nil {
			return err
		}
		if !ok {
			// A hint for a chain we do not run a node for. Ignoring it is the honest
			// behaviour: we will not claim coverage we cannot provide.
			continue
		}

		if !a.Active {
			if err := m.st.SetAssetStatus(ctx, a.ChainID, a.Token, store.StatusRevoked); err != nil {
				return err
			}
			continue
		}

		// A registration names the chain it is about, and getting that wrong is the
		// standing mistake around here — requestIndexing takes a chainId so an index
		// of one chain can be committed on another, and a caller who passes the
		// registry's chain instead funds a key nothing will ever cover.
		//
		// The cost of believing it is not theoretical. An asset registered for a chain
		// whose token has no code there is unindexable by construction: no code, no
		// events, ever. But the indexer does not know that, so it probes the history
		// floor, which binary-searches the whole chain in archive reads, exhausts the
		// RPC quota, dies, and cancels the context the other chains' indexers and the
		// publisher share. One mistyped chainId took the whole daemon down.
		//
		// So check for code before adopting the hint. This is cheap (one eth_call at
		// head), total (a token with no code cannot emit), and it refuses only what
		// could never have worked.
		if indexable, err := m.hasCode(ctx, a.ChainID, a.Token); err != nil {
			m.log.Warn("could not check registered token for code; skipping this round",
				"chain_id", a.ChainID, "token", a.Token.Hex(), "err", err)
			continue
		} else if !indexable {
			// Mark it so the local scan set does not keep it alive from an earlier
			// import, and so this logs once rather than every sync.
			if err := m.st.SetAssetStatus(ctx, a.ChainID, a.Token, store.StatusRevoked); err != nil {
				return err
			}
			m.log.Warn("ignoring registration for a token with no code on that chain",
				"chain_id", a.ChainID, "token", a.Token.Hex(),
				"registrant", a.Registrant.Hex(),
				"note", "almost certainly the indexed chain was confused with the registry's")
			continue
		}

		// The registrant's fromBlock is a floor for scanning, never a source of
		// truth: a bad value only makes our history shallower, never wrong.
		fromBlock := a.FromBlock
		if fromBlock > head {
			fromBlock = 0
		}

		registrant := a.Registrant
		created, err := m.st.RegisterAsset(ctx, store.Asset{
			ChainID:       a.ChainID,
			Address:       a.Token,
			Standard:      KindToStandard(a.Kind),
			HintFromBlock: fromBlock,
			Registrant:    &registrant,
			RegistryKey:   registryKey(a.ChainID, a.Token),
			Source:        store.SourceOnchain,
		}, head)
		if err != nil {
			return err
		}
		if created {
			m.log.Info("new asset hint",
				"chain_id", a.ChainID, "token", a.Token.Hex(),
				"registrant", a.Registrant.Hex(), "anchor_block", head)
			touched[a.ChainID] = true
		}
	}

	m.syncDemand(ctx)

	for chainID := range touched {
		if m.nudge == nil {
			break
		}
		if n, ok := m.nudge(chainID); ok {
			n.Nudge()
		}
	}

	if head, ok, err := m.head(ctx, m.regChID); err == nil && ok {
		return m.st.SetRegistrySyncCursor(ctx, m.regChID, m.client.Address(), head)
	}
	return nil
}

// syncDemand mirrors the registry's vote counts into asset_demand_onchain, for
// every chain the registry lists — not only the ones this daemon runs. A vote for
// a chain nobody indexes yet is the demand that says which chain to add, and it is
// already counted when that chain starts. The contract deduplicates voters itself,
// so what lands here is an aggregate. A registry deployed before votes existed has
// no listDemand; that is logged once and is not an error.
func (m *Mirror) syncDemand(ctx context.Context) {
	votes, err := m.client.ListDemand(ctx, 200)
	if err != nil {
		if !m.demandUnsupported {
			m.demandUnsupported = true
			m.log.Info("registry serves no demand; on-chain votes are not mirrored", "err", err)
		}
		return
	}
	m.demandUnsupported = false
	// The registry of record is the whole truth: whatever it lists replaces what
	// was mirrored before, so a count synced from a registry this deployment has
	// since moved away from does not linger as a phantom voter.
	rows := make([]store.DemandRow, 0, len(votes))
	for _, v := range votes {
		rows = append(rows, store.DemandRow{ChainID: v.ChainID, Address: v.Token, OnchainVoters: v.Voters})
	}
	if err := m.st.ReplaceOnchainDemand(ctx, rows); err != nil {
		m.log.Warn("could not mirror on-chain demand", "err", err)
	}
}

// AssetKey mirrors HintRegistry.assetKey: keccak256(abi.encodePacked(chainId, token)).
func AssetKey(chainID uint64, token common.Address) common.Hash {
	return common.BytesToHash(registryKey(chainID, token))
}

// registryKey is AssetKey as raw bytes, which is how the store keeps it.
func registryKey(chainID uint64, token common.Address) []byte {
	buf := make([]byte, 0, 28)
	for i := 7; i >= 0; i-- {
		buf = append(buf, byte(chainID>>(8*uint(i))))
	}
	buf = append(buf, token.Bytes()...)
	return crypto.Keccak256(buf)
}

// hasCode reports whether the token has bytecode on the chain it was registered
// for. A nil CodeFunc means the check is unavailable, and an unavailable check
// admits the asset: refusing on ignorance would drop good hints.
func (m *Mirror) hasCode(ctx context.Context, chainID uint64, token common.Address) (bool, error) {
	if m.code == nil {
		return true, nil
	}
	code, err := m.code(ctx, chainID, token)
	if err != nil {
		return false, err
	}
	return len(code) > 0, nil
}
