package price

import "github.com/ethereum/go-ethereum/common"

// Well-known deployments, so a chain evm-scan recognises prices out of the box.
//
// These are the same posture as the rest of the project — discover at runtime, do
// not demand a list — applied to the one thing that cannot be discovered: where the
// factories and the registry live. Everything downstream of these addresses (which
// feeds exist, which pools exist, what they say) is read from the chain at call
// time. Each address is echoed in every response that used it, so a consumer can
// check it against the deployment they trust, and any of them can be overridden or
// switched off in config.
//
// A wrong address here fails safe: a contract that is not there answers nothing,
// and a token with no answering source gets no price rather than a wrong one.

var (
	addr = common.HexToAddress

	defaultFeeTiers = []uint32{100, 500, 3000, 10000}

	knownChains = map[uint64]Sources{
		// Ethereum mainnet: the one chain with a Chainlink Feed Registry.
		1: {
			FeedRegistry:  addr("0x47Fb2585D2C56Fe188D0E6ec628a38b74fCeeeDf"),
			NativeUSDFeed: addr("0x5f4eC3Df9cbd43714FE2740f5E3616155c5b8419"),
			V3Factory:     addr("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
			V2Factory:     addr("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"),
			FeeTiers:      defaultFeeTiers,
			QuoteTokens: []QuoteToken{
				{Address: addr("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"), Symbol: "WETH", WrappedNative: true},
				{Address: addr("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"), Symbol: "USDC", AssumeUSDPeg: true},
				{Address: addr("0xdAC17F958D2ee523a2206206994597C13D831ec7"), Symbol: "USDT", AssumeUSDPeg: true},
				{Address: addr("0x6B175474E89094C44Da98b954EedeAC495271d0F"), Symbol: "DAI", AssumeUSDPeg: true},
			},
		},
		// Optimism.
		10: {
			NativeUSDFeed: addr("0x13e3Ee699D1909E989722E753853AE30b17e08c5"),
			V3Factory:     addr("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
			FeeTiers:      defaultFeeTiers,
			Feeds: []FeedHint{
				{Token: addr("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85"), Aggregator: addr("0x16a9FA2FDa030272Ce99B29CF780dFA30361E0f3"), Quote: QuoteUSD},
			},
			QuoteTokens: []QuoteToken{
				{Address: addr("0x4200000000000000000000000000000000000006"), Symbol: "WETH", WrappedNative: true},
				{Address: addr("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85"), Symbol: "USDC", AssumeUSDPeg: true},
			},
		},
		// Base.
		8453: {
			NativeUSDFeed: addr("0x71041dddad3595F9CEd3DcCFBe3D1F4b0a16Bb70"),
			V3Factory:     addr("0x33128a8fC17869897dcE68Ed026d694621f6FDfD"),
			FeeTiers:      defaultFeeTiers,
			Feeds: []FeedHint{
				{Token: addr("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"), Aggregator: addr("0x7e860098F58bBFC8648a4311b374B1D669a2bc6B"), Quote: QuoteUSD},
			},
			QuoteTokens: []QuoteToken{
				{Address: addr("0x4200000000000000000000000000000000000006"), Symbol: "WETH", WrappedNative: true},
				{Address: addr("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"), Symbol: "USDC", AssumeUSDPeg: true},
			},
		},
		// Arbitrum One.
		42161: {
			NativeUSDFeed: addr("0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612"),
			V3Factory:     addr("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
			FeeTiers:      defaultFeeTiers,
			Feeds: []FeedHint{
				{Token: addr("0xaf88d065e77c8cC2239327C5EDb3A432268e5831"), Aggregator: addr("0x50834F3163758fcC1Df9973b6e91f0F0F0434aD3"), Quote: QuoteUSD},
			},
			QuoteTokens: []QuoteToken{
				{Address: addr("0x82aF49447D8a07e3bd95BD0d56f35241523fBaB1"), Symbol: "WETH", WrappedNative: true},
				{Address: addr("0xaf88d065e77c8cC2239327C5EDb3A432268e5831"), Symbol: "USDC", AssumeUSDPeg: true},
			},
		},
	}
)

// Defaults returns the well-known sources for a chain, and whether it has any.
func Defaults(chainID uint64) (Sources, bool) {
	s, ok := knownChains[chainID]
	if !ok {
		return Sources{}, false
	}
	// Copy the slices so a caller merging config on top cannot mutate the table.
	out := s
	out.Feeds = append([]FeedHint(nil), s.Feeds...)
	out.FeeTiers = append([]uint32(nil), s.FeeTiers...)
	out.QuoteTokens = append([]QuoteToken(nil), s.QuoteTokens...)
	return out, true
}

// KnownChains lists the chain ids with built-in defaults.
func KnownChains() []uint64 {
	out := make([]uint64, 0, len(knownChains))
	for id := range knownChains {
		out = append(out, id)
	}
	return out
}
