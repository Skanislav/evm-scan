package price

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A synthetic chain: the routing rules are what is under test, so every source is
// spelled out by hand rather than read from an EVM (contracts/evmtest does that).

var (
	weth  = common.HexToAddress("0x000000000000000000000000000000000000000a")
	usdc  = common.HexToAddress("0x000000000000000000000000000000000000000b")
	tokA  = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	tokB  = common.HexToAddress("0x00000000000000000000000000000000000000bb")
	tokC  = common.HexToAddress("0x00000000000000000000000000000000000000cc")
	tokD  = common.HexToAddress("0x00000000000000000000000000000000000000dd")
	tokE  = common.HexToAddress("0x00000000000000000000000000000000000000ee")
	tokF  = common.HexToAddress("0x00000000000000000000000000000000000000ff")
	agg   = common.HexToAddress("0x0000000000000000000000000000000000000a99")
	ethUS = common.HexToAddress("0x0000000000000000000000000000000000000e99")

	testSources = Sources{
		NativeUSDFeed: ethUS,
		V3Factory:     common.HexToAddress("0xf3"),
		V2Factory:     common.HexToAddress("0xf2"),
		QuoteTokens: []QuoteToken{
			{Address: weth, Symbol: "WETH", WrappedNative: true},
			{Address: usdc, Symbol: "USDC", AssumeUSDPeg: true},
		},
	}
)

const now = uint64(1_700_000_000)

func u8(v uint8) *uint8 { return &v }

func feed(aggr common.Address, quote QuoteKind, answer int64, dec uint8, updatedAt uint64, desc string) Feed {
	return Feed{
		Aggregator: aggr, Quote: quote, Answered: true, Answer: big.NewInt(answer), Decimals: u8(dec),
		UpdatedAt: updatedAt, RoundID: big.NewInt(9), AnsweredInRound: big.NewInt(9), Description: desc,
	}
}

// v3 builds a pool sitting at `tick` with a TWAP over `window` seconds at the same
// tick (cumulatives grow linearly), or spot-only when window is 0.
func v3(pool, t0, t1 common.Address, tick int32, window uint32, liquidity int64) Pool {
	ratio := ratioFromTick(tick)
	sqrt := new(big.Float).SetPrec(256).Sqrt(new(big.Float).SetPrec(256).SetRat(ratio))
	sqrt.Mul(sqrt, new(big.Float).SetInt(q96))
	sqrtX96, _ := sqrt.Int(nil)
	p := Pool{
		Address: pool, Kind: PoolV3, Token0: t0, Token1: t1, Fee: 3000,
		SqrtPriceX96: sqrtX96, Tick: tick, Liquidity: big.NewInt(liquidity),
	}
	if window > 0 {
		p.TWAPWindow = window
		p.TickCumulativeStart = big.NewInt(0)
		p.TickCumulativeEnd = big.NewInt(int64(tick) * int64(window))
	}
	return p
}

func v2(pair, t0, t1 common.Address, r0, r1 *big.Int) Pool {
	return Pool{Address: pair, Kind: PoolV2, Token0: t0, Token1: t1, Reserve0: r0, Reserve1: r1}
}

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
func e6(n int64) *big.Int  { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e6)) }

// tickFor is the tick nearest a raw token1-per-token0 ratio.
func tickFor(ratio float64) int32 { return int32(math.Round(math.Log(ratio) / math.Log(1.0001))) }

func baseSnapshot() *Snapshot {
	native := feed(ethUS, QuoteUSD, 3000_00000000, 8, now-60, "ETH / USD")
	return &Snapshot{
		ChainID: 1, BlockNumber: 100, Timestamp: now, Atomic: true,
		Native: &native,
		Tokens: []TokenSources{
			{Token: weth, IsContract: true, Symbol: "WETH", Decimals: u8(18)},
			{Token: usdc, IsContract: true, Symbol: "USDC", Decimals: u8(6),
				Feeds: []Feed{feed(agg, QuoteUSD, 1_0000_0000, 8, now-300, "USDC / USD")}},
		},
	}
}

func approx(t *testing.T, what string, got *big.Rat, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no price", what)
	}
	g := ratF(got)
	if math.Abs(g-want)/want > tol {
		t.Errorf("%s = %g, want %g", what, g, want)
	}
}

func TestResolveNativeAndWrappedNative(t *testing.T) {
	r := Resolve(baseSnapshot(), testSources, Policy{})
	approx(t, "native", r.Native.USD, 3000, 1e-9)
	if r.Native.Confidence != ConfidenceHigh {
		t.Errorf("native confidence = %s", r.Native.Confidence)
	}
	w := r.Quotes[weth]
	approx(t, "WETH", w.USD, 3000, 1e-9)
	if w.Source() != KindNativeFeed || w.Confidence != ConfidenceHigh {
		t.Errorf("WETH priced via %s at %s", w.Source(), w.Confidence)
	}
	u := r.Quotes[usdc]
	approx(t, "USDC", u.USD, 1, 1e-9)
	if u.Source() != KindChainlink {
		t.Errorf("USDC with a feed priced via %s; the feed must beat the peg", u.Source())
	}
}

func TestResolveChainlinkDirectAndViaNative(t *testing.T) {
	s := baseSnapshot()
	s.Tokens = append(s.Tokens,
		TokenSources{Token: tokA, IsContract: true, Symbol: "A", Decimals: u8(18),
			Feeds: []Feed{feed(agg, QuoteUSD, 5_0000_0000, 8, now-100, "A / USD")}},
		TokenSources{Token: tokB, IsContract: true, Symbol: "B", Decimals: u8(18),
			Feeds: []Feed{feed(agg, QuoteNative, 1e15, 18, now-100, "B / ETH")}},
	)
	r := Resolve(s, testSources, Policy{})

	a := r.Quotes[tokA]
	approx(t, "A", a.USD, 5, 1e-9)
	if a.Confidence != ConfidenceHigh || len(a.Route) != 1 || a.Route[0].Kind != KindChainlink {
		t.Errorf("A: %s via %+v", a.Confidence, a.Route)
	}

	b := r.Quotes[tokB]
	approx(t, "B", b.USD, 3, 1e-9) // 0.001 ETH * 3000
	if b.Confidence != ConfidenceHigh || len(b.Route) != 2 || b.Route[1].Kind != KindNativeFeed {
		t.Errorf("B: %s via %+v", b.Confidence, b.Route)
	}
}

func TestResolvePoolsRouteThroughQuoteTokens(t *testing.T) {
	s := baseSnapshot()
	// C has no feed. A v3 pool against WETH: token0 = WETH (lower address),
	// token1 = C, both 18 decimals, 1 WETH = 1000 C, so 1 C = 0.001 WETH = 3 USD.
	// And a v2 pair against USDC at 2.9 USD with less depth.
	s.Tokens = append(s.Tokens, TokenSources{
		Token: tokC, IsContract: true, Symbol: "C", Decimals: u8(18),
		Pools: []Pool{
			v3(common.HexToAddress("0x31"), weth, tokC, tickFor(1000), 1800, 1e18),
			// USDC(6) is token0, C(18) is token1: 2.9 USDC per C means
			// raw C per raw USDC = (1/2.9) * 1e18 / 1e6.
			v2(common.HexToAddress("0x21"), usdc, tokC, e6(29_000), e18(10_000)),
		},
	})
	r := Resolve(s, testSources, Policy{})

	c := r.Quotes[tokC]
	approx(t, "C", c.USD, 3, 1e-3)
	if c.Confidence != ConfidenceMedium || c.Source() != KindV3TWAP {
		t.Errorf("C: %s via %s", c.Confidence, c.Source())
	}
	if len(c.Route) != 2 || c.Route[1].Kind != KindNativeFeed {
		t.Errorf("C route = %+v", c.Route)
	}
	if len(c.Alternatives) != 2 {
		t.Fatalf("C alternatives = %d", len(c.Alternatives))
	}
	alt := c.Alternatives[1]
	approx(t, "C via v2", alt.USD, 2.9, 1e-9)
	if alt.Confidence != ConfidenceLow || alt.Source() != KindV2Spot {
		t.Errorf("v2 spot must rank low, got %s via %s", alt.Confidence, alt.Source())
	}
	if alt.Route[0].DepthUSD == nil || ratF(alt.Route[0].DepthUSD) != 29_000 {
		t.Errorf("v2 depth = %v, want 29000 USD of USDC", alt.Route[0].DepthUSD)
	}
}

func TestResolveDeeperPoolWinsWithinTier(t *testing.T) {
	s := baseSnapshot()
	s.Tokens = append(s.Tokens, TokenSources{
		Token: tokC, IsContract: true, Symbol: "C", Decimals: u8(18),
		Pools: []Pool{
			v3(common.HexToAddress("0x31"), weth, tokC, tickFor(1000), 1800, 1e12),
			v3(common.HexToAddress("0x32"), weth, tokC, tickFor(1100), 1800, 1e18),
		},
	})
	r := Resolve(s, testSources, Policy{})
	c := r.Quotes[tokC]
	if c.Route[0].Address != common.HexToAddress("0x32") {
		t.Errorf("chose pool %s; the deeper one should win", c.Route[0].Address.Hex())
	}
	approx(t, "C", c.USD, 3000.0/1100, 1e-3)
}

func TestResolveStaleFeedLosesToTWAP(t *testing.T) {
	s := baseSnapshot()
	s.Tokens = append(s.Tokens, TokenSources{
		Token: tokD, IsContract: true, Symbol: "D", Decimals: u8(18),
		Feeds: []Feed{feed(agg, QuoteUSD, 10_0000_0000, 8, now-48*3600, "D / USD")},
		// token0 = USDC(6), token1 = D(18); 1 D = 9 USDC -> raw D per raw USDC = 1e12/9.
		Pools: []Pool{v3(common.HexToAddress("0x33"), usdc, tokD, tickFor(1e12/9), 1200, 1e15)},
	})
	r := Resolve(s, testSources, Policy{})
	d := r.Quotes[tokD]
	approx(t, "D", d.USD, 9, 1e-3)
	if d.Source() != KindV3TWAP || d.Confidence != ConfidenceMedium {
		t.Errorf("D: %s via %s; a fresh TWAP must beat a two-day-old feed", d.Confidence, d.Source())
	}
	if len(d.Alternatives) != 2 || !d.Alternatives[1].Route[0].Stale || d.Alternatives[1].Confidence != ConfidenceLow {
		t.Errorf("stale feed should remain as a low-confidence alternative: %+v", d.Alternatives)
	}

	// With a policy that tolerates a two-day-old round, the feed is back on top.
	r = Resolve(s, testSources, Policy{MaxFeedAge: 72 * 3600 * 1e9})
	if d := r.Quotes[tokD]; d.Source() != KindChainlink || d.Confidence != ConfidenceHigh {
		t.Errorf("D under a lenient policy: %s via %s", d.Confidence, d.Source())
	}
}

func TestResolveSpotAndShortTWAPAreLow(t *testing.T) {
	s := baseSnapshot()
	s.Tokens = append(s.Tokens,
		TokenSources{Token: tokE, IsContract: true, Symbol: "E", Decimals: u8(18),
			Pools: []Pool{v3(common.HexToAddress("0x34"), weth, tokE, tickFor(500), 0, 1e18)}},
		TokenSources{Token: tokF, IsContract: true, Symbol: "F", Decimals: u8(18),
			Pools: []Pool{v3(common.HexToAddress("0x35"), weth, tokF, tickFor(500), 60, 1e18)}},
	)
	r := Resolve(s, testSources, Policy{})

	e := r.Quotes[tokE]
	approx(t, "E", e.USD, 6, 1e-3)
	if e.Source() != KindV3Spot || e.Confidence != ConfidenceLow {
		t.Errorf("E: %s via %s", e.Confidence, e.Source())
	}
	if !strings.Contains(strings.Join(e.Notes, " "), "spot") {
		t.Errorf("a spot price must say so: %v", e.Notes)
	}

	f := r.Quotes[tokF]
	if f.Source() != KindV3TWAP || f.Confidence != ConfidenceLow || f.Route[0].TWAPWindow != 60 {
		t.Errorf("F: a one-minute TWAP is not a TWAP: %s via %s window %d", f.Confidence, f.Source(), f.Route[0].TWAPWindow)
	}
}

func TestResolveAssumedPegCapsAtMedium(t *testing.T) {
	s := baseSnapshot()
	s.Tokens[1].Feeds = nil // USDC without a feed: only its configured peg remains.
	s.Tokens = append(s.Tokens, TokenSources{
		Token: tokC, IsContract: true, Symbol: "C", Decimals: u8(18),
		Pools: []Pool{v3(common.HexToAddress("0x36"), usdc, tokC, tickFor(1e12/2), 1800, 1e15)},
	})
	r := Resolve(s, testSources, Policy{})

	u := r.Quotes[usdc]
	if u.Source() != KindAssumedPeg || u.Confidence != ConfidenceLow {
		t.Errorf("USDC itself: %s via %s", u.Confidence, u.Source())
	}
	c := r.Quotes[tokC]
	approx(t, "C", c.USD, 2, 1e-3)
	if c.Confidence != ConfidenceMedium || c.Route[1].Kind != KindAssumedPeg {
		t.Errorf("C via peg: %s route %+v", c.Confidence, c.Route)
	}
	if !strings.Contains(strings.Join(c.Notes, " "), "assumed") {
		t.Errorf("routing through an assumption must say so: %v", c.Notes)
	}
}

func TestResolveNothingIsNothing(t *testing.T) {
	s := baseSnapshot()
	s.Tokens = append(s.Tokens,
		TokenSources{Token: tokA, IsContract: false},
		TokenSources{Token: tokB, IsContract: true, Symbol: "B", Decimals: u8(18)},
		// A feed that answered zero is not a price of zero.
		TokenSources{Token: tokC, IsContract: true, Symbol: "C", Decimals: u8(18),
			Feeds: []Feed{feed(agg, QuoteUSD, 0, 8, now, "C / USD")}},
		// A pool on a token without decimals cannot be scaled, so it is not priced.
		TokenSources{Token: tokD, IsContract: true, Symbol: "D",
			Pools: []Pool{v3(common.HexToAddress("0x37"), weth, tokD, 0, 1800, 1e18)}},
	)
	r := Resolve(s, testSources, Policy{})
	for _, tok := range []common.Address{tokA, tokB, tokC, tokD} {
		q := r.Quotes[tok]
		if q.USD != nil || q.Confidence != ConfidenceNone || len(q.Notes) == 0 {
			t.Errorf("%s: got price %v (%s) notes %v; expected none with a reason", tok.Hex(), q.USD, q.Confidence, q.Notes)
		}
	}
}

func TestResolveWithoutNativeFeed(t *testing.T) {
	s := baseSnapshot()
	s.Native = nil
	s.Tokens = append(s.Tokens,
		TokenSources{Token: tokB, IsContract: true, Symbol: "B", Decimals: u8(18),
			Feeds: []Feed{feed(agg, QuoteNative, 1e15, 18, now-100, "B / ETH")}},
		TokenSources{Token: tokC, IsContract: true, Symbol: "C", Decimals: u8(18),
			Pools: []Pool{
				v3(common.HexToAddress("0x31"), weth, tokC, tickFor(1000), 1800, 1e18),
				v3(common.HexToAddress("0x36"), usdc, tokC, tickFor(1e12/3), 1800, 1e15),
			}},
	)
	r := Resolve(s, testSources, Policy{})
	if r.Native.USD != nil {
		t.Error("no native feed, yet a native price")
	}
	if r.Quotes[weth].USD != nil || r.Quotes[tokB].USD != nil {
		t.Error("nothing quoted in ETH can be priced without ETH/USD")
	}
	// C still resolves, through USDC's feed.
	c := r.Quotes[tokC]
	approx(t, "C", c.USD, 3, 1e-3)
	if c.Route[1].Address != agg {
		t.Errorf("C should have crossed through USDC's feed, route %+v", c.Route)
	}
}
