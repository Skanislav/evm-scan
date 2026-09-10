// Package config loads runtime configuration from YAML with environment overrides.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"
)

// Config is the whole deployment.
type Config struct {
	Database Database `yaml:"database"`
	API      API      `yaml:"api"`
	Registry Registry `yaml:"registry"`
	Chains   []Chain  `yaml:"chains"`
	// Cost prices the RPC bill. Providers meter per request, so what a deployment
	// costs is a function of how many it makes; this turns the counters into money.
	Cost Cost `yaml:"cost"`
}

// Cost describes a provider's metering so the daemon can price its own traffic.
//
// Defaults are dRPC's, measured rather than assumed: 32,780 CU over 1,639 requests
// and 40,780 over 2,039 both give exactly 20 CU per request, on two chains, so the
// unit is flat per call and not per method. Set these to whatever the provider in
// use actually charges.
type Cost struct {
	CUPerRequest    float64 `yaml:"cu_per_request"`
	USDPerMillionCU float64 `yaml:"usd_per_million_cu"`
}

// Rate returns the dollars a single request costs, or 0 when unpriced.
func (c Cost) Rate() float64 {
	if c.CUPerRequest <= 0 || c.USDPerMillionCU <= 0 {
		return 0
	}
	return c.CUPerRequest * c.USDPerMillionCU / 1e6
}

type Database struct {
	DSN string `yaml:"dsn"`
	// AutoMigrate applies embedded migrations on start.
	AutoMigrate bool `yaml:"auto_migrate"`
}

type API struct {
	Listen string `yaml:"listen"`
	// CORSOrigin is echoed as Access-Control-Allow-Origin. Empty disables CORS.
	CORSOrigin string `yaml:"cors_origin"`
	// AllowRegistration lets the HTTP API add asset hints directly, bypassing the
	// on-chain registry. Convenient for local work; the on-chain path is the real one.
	AllowRegistration bool `yaml:"allow_registration"`
	// AuthToken guards the endpoints that spend something: publishing an epoch costs
	// the publisher's gas, and promoting an asset costs a backfill against a paid RPC
	// quota. Empty leaves them open, which is right for a laptop and wrong for a
	// public URL with a funded publisher behind it. Prefer EVMSCAN_API_TOKEN.
	AuthToken string `yaml:"auth_token"`
}

// Registry points at the deployed HintRegistry.
type Registry struct {
	ChainID uint64 `yaml:"chain_id"`
	Address string `yaml:"address"`
	// PublisherKey is a hex private key used to sign commitments. Prefer the
	// EVMSCAN_PUBLISHER_KEY environment variable over writing it to disk.
	PublisherKey string   `yaml:"publisher_key"`
	SyncInterval Duration `yaml:"sync_interval"`
	// AutoPublishInterval > 0 builds and posts a commitment on a timer.
	AutoPublishInterval Duration `yaml:"auto_publish_interval"`
	// CommitmentURI is recorded alongside a published root, pointing at the full table.
	CommitmentURI string `yaml:"commitment_uri"`
	// Publisher controls how commitments reach the chain.
	Publisher PublisherCfg `yaml:"publisher"`
}

// PublisherCfg selects and tunes the transaction submitter.
//
// Gas is deliberately behind an interface (hintreg.Submitter). Today the only mode is
// an EOA that pays for itself and is paid back, per block of funded coverage, when it
// claims against its finalized epochs; a paymaster or relayer would be another mode
// here and nothing else.
type PublisherCfg struct {
	// Mode is "eoa" (the default when empty). Other values are reserved.
	Mode string `yaml:"mode"`
	// SubmitTimeout bounds how long one submission is waited on before the wait is
	// abandoned and left for ResumePending.
	SubmitTimeout Duration `yaml:"submit_timeout"`
	// StaleAfter is how long an unconfirmed submission may linger before it is
	// written off as failed.
	StaleAfter Duration `yaml:"stale_after"`
	// FallbackGas is used when the node cannot estimate gas. Zero means fail. A
	// light client without eth_estimateGas is the case it exists for.
	FallbackGas uint64 `yaml:"fallback_gas"`
	// MinExpectedReward, in wei, is the least the registry must quote for an
	// epoch's coverage before it is posted. Zero posts regardless. This is the gas
	// floor: with it set, an unfunded chain does not get epochs.
	MinExpectedReward string `yaml:"min_expected_reward_wei"`
}

// Chain is one indexed network.
type Chain struct {
	ChainID uint64 `yaml:"chain_id"`
	Name    string `yaml:"name"`
	// Node is an IPC path, ws:// or http:// URL for *our own* execution client.
	Node string `yaml:"node"`
	// RequireLocalNode refuses to start against a non-loopback endpoint. On by
	// default: the point of running our own snap-synced node is not depending on a
	// third party, and that guarantee should fail loudly rather than silently.
	RequireLocalNode *bool `yaml:"require_local_node"`
	// Confirmations is the depth at which events are folded into the rollup.
	Confirmations    uint64   `yaml:"confirmations"`
	BackfillWindow   uint64   `yaml:"backfill_window"`
	TailWindow       uint64   `yaml:"tail_window"`
	PollInterval     Duration `yaml:"poll_interval"`
	BackfillInterval Duration `yaml:"backfill_interval"`
	// Discovery finds contracts by watching the head, rather than requiring every
	// contract to be registered up front.
	Discovery Discovery `yaml:"discovery"`
	// Pricing reads prices from on-chain oracles and DEX pools through the node,
	// never from a quote API.
	Pricing Pricing `yaml:"pricing"`
}

// Discovery configures the head-watching sweep.
//
// Discovery is intentionally forward-looking: it never walks history. History is the
// expensive resource, and this design only spends it on contracts something has
// already decided are worth indexing.
type Discovery struct {
	// Enabled turns on the sweep. Off means only registered assets are ever indexed.
	Enabled bool `yaml:"enabled"`
	// Lookback is how far behind the head a cold start begins.
	Lookback uint64 `yaml:"lookback"`
	// MaxBlocksPerTick bounds catch-up so discovery cannot starve the indexer.
	MaxBlocksPerTick uint64   `yaml:"max_blocks_per_tick"`
	Interval         Duration `yaml:"interval"`

	// AutoPromote lets activity thresholds alone commit a contract to being indexed.
	// Off by default: promotion costs a full backfill, and the on-chain registry is
	// the authoritative signal for "this contract matters".
	AutoPromote          bool   `yaml:"auto_promote"`
	MinEvents            uint64 `yaml:"min_events"`
	MinBlocks            uint64 `yaml:"min_blocks"`
	MaxPromotionsPerTick int    `yaml:"max_promotions_per_tick"`
}

// Pricing configures on-chain price discovery for one chain.
//
// Every field is optional. A chain evm-scan knows (mainnet, Optimism, Base,
// Arbitrum) gets its well-known Feed Registry, native/USD feed, Uniswap factories
// and quote tokens filled in; anything set here overrides the matching default,
// and use_defaults: false drops the built-ins entirely.
type Pricing struct {
	// Enabled defaults to true. Pricing that has no source at all is inert anyway.
	Enabled *bool `yaml:"enabled"`
	// UseDefaults merges the chain's built-in sources under this config. Default true.
	UseDefaults *bool `yaml:"use_defaults"`
	// FeedRegistry is Chainlink's Feed Registry (mainnet only).
	FeedRegistry string `yaml:"feed_registry"`
	// NativeUSDFeed is the native asset's USD aggregator (ETH/USD on Ethereum).
	NativeUSDFeed string `yaml:"native_usd_feed"`
	// Feeds pin aggregators to tokens, for chains without a registry.
	Feeds []PriceFeed `yaml:"feeds"`
	// UniswapV3Factory and UniswapV2Factory are the DEX factories to discover pools
	// through; any getPool/getPair-compatible fork works.
	UniswapV3Factory string `yaml:"uniswap_v3_factory"`
	UniswapV2Factory string `yaml:"uniswap_v2_factory"`
	// FeeTiers are the v3 fee tiers to probe. Default 100, 500, 3000, 10000.
	FeeTiers []uint32 `yaml:"fee_tiers"`
	// QuoteTokens are the pool counterparties to look for.
	QuoteTokens []QuoteToken `yaml:"quote_tokens"`
	// TWAPWindow is asked of every v3 pool. Default 30m.
	TWAPWindow Duration `yaml:"twap_window"`
	// MinTWAPWindow is the shortest window that still counts as a TWAP rather than
	// a spot price. Default 10m.
	MinTWAPWindow Duration `yaml:"min_twap_window"`
	// MaxFeedAge is how old a Chainlink round may be before it is reported stale
	// and outranked by a DEX TWAP. Default 25h.
	MaxFeedAge Duration `yaml:"max_feed_age"`
	// CacheTTL is how long a quote is reused. Default 12s, about one block.
	CacheTTL Duration `yaml:"cache_ttl"`
}

// PriceFeed binds a token to a Chainlink aggregator.
type PriceFeed struct {
	Token      string `yaml:"token"`
	Aggregator string `yaml:"aggregator"`
	// Quote is "usd" (default) or "native" (ETH on Ethereum).
	Quote string `yaml:"quote"`
}

// QuoteToken is a DEX pool counterparty.
type QuoteToken struct {
	Address string `yaml:"address"`
	Symbol  string `yaml:"symbol"`
	// WrappedNative prices the token off the native/USD feed.
	WrappedNative bool `yaml:"wrapped_native"`
	// AssumeUSDPeg treats the token as 1 USD when no feed covers it.
	AssumeUSDPeg bool `yaml:"assume_usd_peg"`
}

// PricingEnabled resolves the tri-state flag, defaulting to true.
func (c Chain) PricingEnabled() bool {
	return c.Pricing.Enabled == nil || *c.Pricing.Enabled
}

// MergeDefaults resolves the use_defaults flag, defaulting to true.
func (p Pricing) MergeDefaults() bool {
	return p.UseDefaults == nil || *p.UseDefaults
}

// RequireLocal resolves the tri-state flag, defaulting to true.
func (c Chain) RequireLocal() bool {
	if c.RequireLocalNode == nil {
		return true
	}
	return *c.RequireLocalNode
}

// Load reads a YAML file, applies environment overrides and validates the result.
func Load(path string) (*Config, error) {
	cfg := &Config{
		API:      API{Listen: "127.0.0.1:8080"},
		Database: Database{AutoMigrate: true},
		Registry: Registry{
			SyncInterval: Duration(10 * time.Second),
			Publisher: PublisherCfg{
				SubmitTimeout: Duration(3 * time.Minute),
				StaleAfter:    Duration(30 * time.Minute),
			},
		},
	}

	if path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := yaml.Unmarshal(body, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv lets deployment secrets stay out of the config file.
func (c *Config) applyEnv() {
	if v := os.Getenv("EVMSCAN_DATABASE_DSN"); v != "" {
		c.Database.DSN = v
	}
	if v := os.Getenv("EVMSCAN_API_LISTEN"); v != "" {
		c.API.Listen = v
	}
	if v := os.Getenv("EVMSCAN_PUBLISHER_KEY"); v != "" {
		c.Registry.PublisherKey = v
	}
	if v := os.Getenv("EVMSCAN_API_TOKEN"); v != "" {
		c.API.AuthToken = v
	}
	if v := os.Getenv("EVMSCAN_REGISTRY_ADDRESS"); v != "" {
		c.Registry.Address = v
	}
	// A single-chain deployment can be configured entirely from the environment,
	// which is what the demo and container images use.
	if v := os.Getenv("EVMSCAN_NODE"); v != "" && len(c.Chains) > 0 {
		c.Chains[0].Node = v
	}
	// EVMSCAN_NODE_<chain id> names a chain explicitly, which is what a deployment
	// with more than one needs: a registry on an L2 is reached over a URL with a key
	// in it, and that cannot live in a committed profile.
	for i := range c.Chains {
		if v := os.Getenv(fmt.Sprintf("EVMSCAN_NODE_%d", c.Chains[i].ChainID)); v != "" {
			c.Chains[i].Node = v
		}
	}
}

func (c *Config) validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("config: database.dsn is required (or set EVMSCAN_DATABASE_DSN)")
	}
	if len(c.Chains) == 0 {
		return fmt.Errorf("config: at least one chain must be configured")
	}

	seen := map[uint64]bool{}
	for i, ch := range c.Chains {
		if ch.ChainID == 0 {
			return fmt.Errorf("config: chains[%d].chain_id is required", i)
		}
		if seen[ch.ChainID] {
			return fmt.Errorf("config: chain_id %d configured more than once", ch.ChainID)
		}
		seen[ch.ChainID] = true
		if ch.Node == "" {
			return fmt.Errorf("config: chains[%d] (chain_id %d) needs a node endpoint", i, ch.ChainID)
		}
		if err := ch.Pricing.validate(); err != nil {
			return fmt.Errorf("config: chains[%d].pricing: %w", i, err)
		}
	}

	if c.Registry.Address != "" {
		if !common.IsHexAddress(c.Registry.Address) {
			return fmt.Errorf("config: registry.address %q is not an address", c.Registry.Address)
		}
		if c.Registry.ChainID == 0 {
			return fmt.Errorf("config: registry.chain_id is required when registry.address is set")
		}
		if !seen[c.Registry.ChainID] {
			return fmt.Errorf("config: registry.chain_id %d is not among the configured chains",
				c.Registry.ChainID)
		}
	}

	if c.Registry.PublisherKey != "" {
		k := strings.TrimPrefix(c.Registry.PublisherKey, "0x")
		if len(k) != 64 {
			return fmt.Errorf("config: registry.publisher_key must be a 32-byte hex key")
		}
	}
	switch c.Registry.Publisher.Mode {
	case "", PublisherModeEOA:
	default:
		return fmt.Errorf("config: registry.publisher.mode %q is not implemented (only %q)",
			c.Registry.Publisher.Mode, PublisherModeEOA)
	}
	return nil
}

// PublisherModeEOA signs with registry.publisher_key and pays its own gas.
const PublisherModeEOA = "eoa"

// RegistryAddress returns the parsed registry address and whether one is configured.
func (c *Config) RegistryAddress() (common.Address, bool) {
	if c.Registry.Address == "" {
		return common.Address{}, false
	}
	return common.HexToAddress(c.Registry.Address), true
}

func (p Pricing) validate() error {
	check := func(name, v string) error {
		if v != "" && !common.IsHexAddress(v) {
			return fmt.Errorf("%s %q is not an address", name, v)
		}
		return nil
	}
	for name, v := range map[string]string{
		"feed_registry": p.FeedRegistry, "native_usd_feed": p.NativeUSDFeed,
		"uniswap_v3_factory": p.UniswapV3Factory, "uniswap_v2_factory": p.UniswapV2Factory,
	} {
		if err := check(name, v); err != nil {
			return err
		}
	}
	for i, f := range p.Feeds {
		if !common.IsHexAddress(f.Token) || !common.IsHexAddress(f.Aggregator) {
			return fmt.Errorf("feeds[%d] needs a token and an aggregator address", i)
		}
		switch strings.ToLower(f.Quote) {
		case "", "usd", "native", "eth":
		default:
			return fmt.Errorf("feeds[%d].quote %q must be usd or native", i, f.Quote)
		}
	}
	for i, q := range p.QuoteTokens {
		if !common.IsHexAddress(q.Address) {
			return fmt.Errorf("quote_tokens[%d].address %q is not an address", i, q.Address)
		}
	}
	return nil
}
