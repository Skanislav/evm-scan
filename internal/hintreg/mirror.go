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

// Nudger lets the mirror wake a chain's follower after new registrations.
type Nudger interface{ Nudge() }

// Mirror pulls permissionlessly registered hints into the local scan set.
//
// Registration is open by design: anyone can point the indexer at any contract. That
// is safe precisely because a hint confers nothing — it only asks us to read public
// logs. Everything downstream treats registrant-supplied fields as untrusted.
type Mirror struct {
	client  *Client
	st      *store.Store
	head    HeadFunc
	nudge   map[uint64]Nudger
	log     *slog.Logger
	regChID uint64
}

// NewMirror builds a Mirror. regChainID is the chain the registry is deployed on,
// which need not be a chain we index.
func NewMirror(c *Client, st *store.Store, regChainID uint64, head HeadFunc, nudge map[uint64]Nudger, log *slog.Logger) *Mirror {
	return &Mirror{
		client:  c,
		st:      st,
		head:    head,
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

	for chainID := range touched {
		if n, ok := m.nudge[chainID]; ok {
			n.Nudge()
		}
	}

	if head, ok, err := m.head(ctx, m.regChID); err == nil && ok {
		return m.st.SetRegistrySyncCursor(ctx, m.regChID, m.client.Address(), head)
	}
	return nil
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
