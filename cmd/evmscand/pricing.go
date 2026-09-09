package main

import (
	"log/slog"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/config"
	"github.com/Skanislav/evm-scan/internal/price"
)

// newPricer builds one chain's pricer from its config layered over the chain's
// built-in defaults. It returns nil when pricing is off or has nothing to read.
func newPricer(src chain.Source, c config.Chain, log *slog.Logger) *price.Pricer {
	if !c.PricingEnabled() {
		return nil
	}
	pc := c.Pricing

	var sources price.Sources
	if pc.MergeDefaults() {
		sources, _ = price.Defaults(c.ChainID)
	}
	if pc.FeedRegistry != "" {
		sources.FeedRegistry = common.HexToAddress(pc.FeedRegistry)
	}
	if pc.NativeUSDFeed != "" {
		sources.NativeUSDFeed = common.HexToAddress(pc.NativeUSDFeed)
	}
	if pc.UniswapV3Factory != "" {
		sources.V3Factory = common.HexToAddress(pc.UniswapV3Factory)
	}
	if pc.UniswapV2Factory != "" {
		sources.V2Factory = common.HexToAddress(pc.UniswapV2Factory)
	}
	if len(pc.FeeTiers) > 0 {
		sources.FeeTiers = pc.FeeTiers
	}
	for _, f := range pc.Feeds {
		q := price.QuoteUSD
		if s := strings.ToLower(f.Quote); s == "native" || s == "eth" {
			q = price.QuoteNative
		}
		sources.Feeds = append(sources.Feeds, price.FeedHint{
			Token: common.HexToAddress(f.Token), Aggregator: common.HexToAddress(f.Aggregator), Quote: q,
		})
	}
	// Configured quote tokens replace the defaults rather than adding to them: the
	// list is a preference order, and an operator who writes one means it.
	if len(pc.QuoteTokens) > 0 {
		sources.QuoteTokens = nil
		for _, q := range pc.QuoteTokens {
			sources.QuoteTokens = append(sources.QuoteTokens, price.QuoteToken{
				Address: common.HexToAddress(q.Address), Symbol: q.Symbol,
				WrappedNative: q.WrappedNative, AssumeUSDPeg: q.AssumeUSDPeg,
			})
		}
	}
	if sources.Empty() {
		return nil
	}

	return price.New(src, price.Config{
		Sources: sources,
		Policy: price.Policy{
			MaxFeedAge:    pc.MaxFeedAge.D(),
			MinTWAPWindow: pc.MinTWAPWindow.D(),
		},
		TWAPWindow: pc.TWAPWindow.D(),
		CacheTTL:   pc.CacheTTL.D(),
	}, log)
}
