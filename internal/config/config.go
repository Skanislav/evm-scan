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
	if v := os.Getenv("EVMSCAN_REGISTRY_ADDRESS"); v != "" {
		c.Registry.Address = v
	}
	// A single-chain deployment can be configured entirely from the environment,
	// which is what the demo and container images use.
	if v := os.Getenv("EVMSCAN_NODE"); v != "" && len(c.Chains) > 0 {
		c.Chains[0].Node = v
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
