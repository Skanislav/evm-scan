package api

import (
	"context"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/hintreg"
)

// registryJSON is the registry block of /v1/status: where the contract is, what it
// charges, and — because verifying a root is only worth something if disagreeing
// with it does something — who settles a dispute and on what terms. Everything
// below chain_id and address is an immutable of the deployment; changing any of
// it means a new registry, which is why the page can state it as a rule rather
// than as a setting.
type registryJSON struct {
	ChainID uint64 `json:"chain_id"`
	Address string `json:"address"`
	// RewardPerBlockWei is what the registry pays a publisher per newly covered
	// block of a funded asset; MinFundingWei is the least a request deposits.
	RewardPerBlockWei string `json:"reward_per_block_wei,omitempty"`
	MinFundingWei     string `json:"min_funding_wei,omitempty"`
	// AssetBondWei is what registerAsset locks for an asset nobody has
	// registered yet. requestIndexing wants assetBond + minFunding for a new
	// asset and minFunding for one that already exists, so a caller building
	// that transaction needs both numbers.
	AssetBondWei string `json:"asset_bond_wei,omitempty"`
	// ENSParent is the name a HintResolver serves the index under, so a
	// reader knows an account's hint name exists before asking about one.
	// Absent when no resolver is attached.
	ENSParent string `json:"ens_parent,omitempty"`

	// Mode is "oracle" when UMA's Optimistic Oracle V3 settles disputes and
	// "local-arbiter" when one key does. Absent until the registry has answered.
	Mode string `json:"mode,omitempty"`
	// Oracle is the oracle's address in oracle mode; Arbiter the settling key in
	// local-arbiter mode. Each is absent in the other mode, because the contract
	// holds the zero address there and a page should not have to know that.
	Oracle  string `json:"oracle,omitempty"`
	Arbiter string `json:"arbiter,omitempty"`
	// PublisherBond is what a commitment stakes and what a challenger matches:
	// units of BondCurrency in oracle mode, wei in local-arbiter mode.
	// BondCurrencySymbol and BondCurrencyDecimals are the token's own answers,
	// present when it gave them, so the bond can be shown as an amount.
	PublisherBond        string `json:"publisher_bond,omitempty"`
	BondCurrency         string `json:"bond_currency,omitempty"`
	BondCurrencySymbol   string `json:"bond_currency_symbol,omitempty"`
	BondCurrencyDecimals *uint8 `json:"bond_currency_decimals,omitempty"`
	// ChallengeWindowSeconds is how long a commitment stays disputable after it
	// is published; the assertion liveness in oracle mode.
	ChallengeWindowSeconds uint64 `json:"challenge_window_seconds,omitempty"`
}

// registryRules is what the registry said about itself, kept once it has answered
// in full. Partial answers are not kept: a status that named the bond but not the
// mode would have the page guessing which currency the bond is in.
type registryRules struct {
	mode           hintreg.Mode
	rewardPerBlock string
	minFunding     string
	symbol         string
	decimals       *uint8
}

// registryStatus builds the registry block, reading the contract only until it
// has answered once. Nil when the deployment names no registry.
func (s *Server) registryStatus(ctx context.Context) *registryJSON {
	if s.d.Registry == nil {
		return nil
	}
	out := &registryJSON{ChainID: s.d.RegistryChainID, Address: s.d.Registry.Address().Hex(), ENSParent: s.d.ENSParent}

	rules := s.registryRules(ctx)
	if rules == nil {
		return out
	}
	m := rules.mode
	out.RewardPerBlockWei = rules.rewardPerBlock
	out.MinFundingWei = rules.minFunding
	out.AssetBondWei = m.AssetBond.String()
	out.PublisherBond = m.PublisherBond.String()
	out.ChallengeWindowSeconds = m.ChallengeWindow
	if m.OracleMode() {
		out.Mode = "oracle"
		out.Oracle = m.Oracle.Hex()
		out.BondCurrency = m.BondCurrency.Hex()
		out.BondCurrencySymbol = rules.symbol
		out.BondCurrencyDecimals = rules.decimals
	} else {
		out.Mode = "local-arbiter"
		out.Arbiter = m.Arbiter.Hex()
	}
	return out
}

func (s *Server) registryRules(ctx context.Context) *registryRules {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if s.reg != nil {
		return s.reg
	}
	mode, err := s.d.Registry.Mode(ctx)
	if err != nil {
		return nil
	}
	rpb, err := s.d.Registry.RewardPerBlock(ctx)
	if err != nil {
		return nil
	}
	minF, err := s.d.Registry.MinFunding(ctx)
	if err != nil {
		return nil
	}
	r := &registryRules{mode: mode, rewardPerBlock: rpb.String(), minFunding: minF.String()}
	if mode.BondCurrency != (common.Address{}) {
		r.symbol, r.decimals = s.d.Registry.ERC20Meta(ctx, mode.BondCurrency)
	}
	s.reg = r
	return r
}
