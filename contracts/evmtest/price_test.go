package evmtest_test

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/price"
)

// Chainlink's Feed Registry denominations, as PriceLens keys them.
var (
	denomUSD = common.BigToAddress(big.NewInt(840))
	denomETH = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")
)

// priceFixture is a small chain with every kind of source the lens knows how to
// find: a registry-resolved feed, a pinned feed, a v3 pool with oracle history and
// a v2 pair, arranged so a token nobody configured a feed for still gets a price.
type priceFixture struct {
	h                        *harness
	weth, usdc, stealth, dud common.Address
	registry, ethUsd         common.Address
	v3Factory, v3Pool        common.Address
	v2Factory, v2Pair        common.Address
	sources                  price.Sources
	stealthPerWeth           float64
}

func newPriceFixture(t *testing.T) *priceFixture {
	t.Helper()
	h := newHarness(t)
	f := &priceFixture{h: h, stealthPerWeth: 1000}

	f.weth = h.deploy("DemoERC20", "Demo Wrapped Ether", "dWETH")
	f.usdc = h.deploy("DemoERC20", "Demo USD Coin", "dUSDC")
	f.stealth = h.deploy("DemoERC20", "Demo Stealth Token", "dSTEALTH")
	f.dud = h.deploy("DemoERC20", "No Market At All", "DUD")

	// Feeds: ETH/USD at 3000 and USDC/USD at 1, both 8 decimals like the real ones,
	// both reachable only through the registry.
	f.ethUsd = h.deploy("MockAggregatorV3", uint8(8), "ETH / USD", big.NewInt(3000_00000000))
	usdcUsd := h.deploy("MockAggregatorV3", uint8(8), "USDC / USD", big.NewInt(1_00000000))
	f.registry = h.deploy("MockFeedRegistry")
	h.send(f.registry, "setFeed", denomETH, denomUSD, f.ethUsd)
	h.send(f.registry, "setFeed", f.usdc, denomUSD, usdcUsd)

	// v3: STEALTH/WETH at 0.3%, 1 WETH = 1000 STEALTH, deep liquidity, an hour of
	// oracle history. Token order is whatever the addresses dictate.
	t0, t1 := f.weth, f.stealth
	ratio := f.stealthPerWeth // token1 (STEALTH) per token0 (WETH)
	if t1.Cmp(t0) < 0 {
		t0, t1 = t1, t0
		ratio = 1 / ratio
	}
	tick := price.TickForRatio(ratio)
	f.v3Pool = h.deploy("MockUniswapV3Pool", t0, t1, big.NewInt(3000),
		price.SqrtPriceX96FromTick(tick), big.NewInt(int64(tick)), new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil))
	h.send(f.v3Pool, "setHistory", uint32(h.timestamp()-3600), uint16(1))
	f.v3Factory = h.deploy("MockUniswapV3Factory")
	h.send(f.v3Factory, "register", f.v3Pool)

	// v2: STEALTH/USDC at 2.9 USDC per STEALTH, shallower.
	p0, p1 := f.stealth, f.usdc
	r0, r1 := e18(10_000), e18(29_000)
	if p1.Cmp(p0) < 0 {
		p0, p1 = p1, p0
		r0, r1 = r1, r0
	}
	f.v2Pair = h.deploy("MockUniswapV2Pair", p0, p1, r0, r1)
	f.v2Factory = h.deploy("MockUniswapV2Factory")
	h.send(f.v2Factory, "register", f.v2Pair)

	f.sources = price.Sources{
		FeedRegistry: f.registry,
		V3Factory:    f.v3Factory,
		V2Factory:    f.v2Factory,
		QuoteTokens: []price.QuoteToken{
			{Address: f.weth, Symbol: "dWETH", WrappedNative: true},
			{Address: f.usdc, Symbol: "dUSDC", AssumeUSDPeg: true},
		},
	}
	return f
}

func e18(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func (h *harness) timestamp() uint64 {
	h.t.Helper()
	head, err := h.client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return head.Time
}

func within(t *testing.T, what string, got *big.Rat, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no price", what)
	}
	g, _ := got.Float64()
	if math.Abs(g-want)/want > tol {
		t.Errorf("%s = %g, want %g", what, g, want)
	}
}

// TestPriceLensAgainstEVM runs the compiled PriceLens in a real EVM: registry
// lookups, feed reads, factory lookups, slot0/observations/observe and getReserves,
// all through one eth_call with no `to` address.
func TestPriceLensAgainstEVM(t *testing.T) {
	f := newPriceFixture(t)
	defer f.h.close()

	snap, err := price.Query(context.Background(), f.h.src(), price.Request{
		Tokens:     []common.Address{f.stealth, f.weth, f.usdc, f.dud, common.HexToAddress("0xdead")},
		Sources:    f.sources,
		TWAPWindow: 1800,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if snap.Calls != 1 || !snap.Atomic {
		t.Errorf("calls=%d atomic=%v", snap.Calls, snap.Atomic)
	}
	if got, want := snap.BlockNumber, f.h.head(); got != want {
		t.Errorf("block = %d, head = %d", got, want)
	}

	// Native: resolved through the registry, since no explicit feed was named.
	if snap.Native == nil || !snap.Native.ViaRegistry || snap.Native.Aggregator != f.ethUsd {
		t.Fatalf("native = %+v", snap.Native)
	}
	if !snap.Native.Answered || snap.Native.Answer.Int64() != 3000_00000000 || snap.Native.Description != "ETH / USD" {
		t.Errorf("native feed = %+v", snap.Native)
	}
	if snap.Native.Decimals == nil || *snap.Native.Decimals != 8 {
		t.Errorf("native decimals = %v", snap.Native.Decimals)
	}

	by := map[common.Address]price.TokenSources{}
	for _, tok := range snap.Tokens {
		by[tok.Token] = tok
	}

	// STEALTH: no feed, one v3 pool with a full TWAP window, one v2 pair.
	st := by[f.stealth]
	if st.Symbol != "dSTEALTH" || st.Decimals == nil || *st.Decimals != 18 {
		t.Errorf("stealth metadata = %+v", st)
	}
	if len(st.Feeds) != 0 {
		t.Errorf("stealth has feeds: %+v", st.Feeds)
	}
	if len(st.Pools) != 2 {
		t.Fatalf("stealth pools = %+v", st.Pools)
	}
	var v3, v2 *price.Pool
	for i := range st.Pools {
		switch st.Pools[i].Kind {
		case price.PoolV3:
			v3 = &st.Pools[i]
		case price.PoolV2:
			v2 = &st.Pools[i]
		}
	}
	if v3 == nil || v2 == nil {
		t.Fatalf("expected one v3 and one v2 pool, got %+v", st.Pools)
	}
	if v3.Address != f.v3Pool || v3.Fee != 3000 || v3.Liquidity.Sign() <= 0 || v3.SqrtPriceX96.Sign() <= 0 {
		t.Errorf("v3 = %+v", v3)
	}
	if v3.TWAPWindow != 1800 {
		t.Errorf("twap window = %d, want the full 1800s the oracle held", v3.TWAPWindow)
	}
	// The mock's tick has been constant, so the cumulative difference over the
	// window must be exactly tick * window.
	delta := new(big.Int).Sub(v3.TickCumulativeEnd, v3.TickCumulativeStart)
	if delta.Int64() != int64(v3.Tick)*1800 {
		t.Errorf("cumulative delta = %s, want tick %d * 1800", delta, v3.Tick)
	}
	if v2.Address != f.v2Pair || v2.Reserve0.Sign() <= 0 || v2.Reserve1.Sign() <= 0 {
		t.Errorf("v2 = %+v", v2)
	}

	// USDC: one feed, found through the registry.
	uc := by[f.usdc]
	if len(uc.Feeds) != 1 || !uc.Feeds[0].ViaRegistry || uc.Feeds[0].Quote != price.QuoteUSD ||
		uc.Feeds[0].Answer.Int64() != 1_00000000 {
		t.Errorf("usdc feeds = %+v", uc.Feeds)
	}
	// WETH and USDC are quote tokens, so they are never paired with themselves; the
	// only pools they appear in are each other's, and there are none.
	if len(by[f.weth].Pools) != 0 || len(uc.Pools) != 0 {
		t.Errorf("quote tokens have pools: weth=%+v usdc=%+v", by[f.weth].Pools, uc.Pools)
	}
	// DUD: a contract with nothing behind it. 0xdead: not even a contract.
	if d := by[f.dud]; !d.IsContract || len(d.Feeds) != 0 || len(d.Pools) != 0 {
		t.Errorf("dud = %+v", d)
	}
	if d := by[common.HexToAddress("0xdead")]; d.IsContract || d.Symbol != "" || d.Decimals != nil {
		t.Errorf("0xdead = %+v", d)
	}

	// Now the part that turns reads into prices.
	res := price.Resolve(snap, f.sources, price.Policy{})
	within(t, "native", res.Native.USD, 3000, 1e-9)
	within(t, "dWETH", res.Quotes[f.weth].USD, 3000, 1e-9)
	within(t, "dUSDC", res.Quotes[f.usdc].USD, 1, 1e-9)
	if q := res.Quotes[f.usdc]; q.Confidence != price.ConfidenceHigh || q.Source() != price.KindChainlink {
		t.Errorf("dUSDC: %s via %s", q.Confidence, q.Source())
	}

	q := res.Quotes[f.stealth]
	within(t, "dSTEALTH", q.USD, 3000/f.stealthPerWeth, 1e-3)
	if q.Confidence != price.ConfidenceMedium || q.Source() != price.KindV3TWAP {
		t.Errorf("dSTEALTH: %s via %s, want medium via TWAP", q.Confidence, q.Source())
	}
	if len(q.Route) != 2 || q.Route[0].Address != f.v3Pool || q.Route[1].Kind != price.KindNativeFeed {
		t.Errorf("dSTEALTH route = %+v", q.Route)
	}
	if len(q.Alternatives) != 2 || q.Alternatives[1].Source() != price.KindV2Spot {
		t.Fatalf("dSTEALTH alternatives = %+v", q.Alternatives)
	}
	within(t, "dSTEALTH via v2", q.Alternatives[1].USD, 2.9, 1e-9)

	if d := res.Quotes[f.dud]; d.USD != nil || d.Confidence != price.ConfidenceNone {
		t.Errorf("DUD priced at %v", d.USD)
	}
}

// TestPriceLensFallsBackToSpotWithoutOracleHistory: a pool whose observation
// buffer was never grown has nothing to average. The lens must say so (window 0)
// and the resolver must label the result as the spot it is.
func TestPriceLensFallsBackToSpotWithoutOracleHistory(t *testing.T) {
	f := newPriceFixture(t)
	defer f.h.close()

	f.h.send(f.v3Pool, "setHistory", uint32(f.h.timestamp()), uint16(0))

	snap, err := price.Query(context.Background(), f.h.src(), price.Request{
		Tokens: []common.Address{f.stealth, f.weth}, Sources: f.sources, TWAPWindow: 1800,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var v3 *price.Pool
	for i := range snap.Tokens[0].Pools {
		if snap.Tokens[0].Pools[i].Kind == price.PoolV3 {
			v3 = &snap.Tokens[0].Pools[i]
		}
	}
	if v3 == nil || v3.TWAPWindow != 0 {
		t.Fatalf("v3 = %+v, want a pool with no TWAP window", v3)
	}

	res := price.Resolve(snap, f.sources, price.Policy{})
	q := res.Quotes[f.stealth]
	within(t, "dSTEALTH spot", q.USD, 3000/f.stealthPerWeth, 1e-3)
	if q.Confidence != price.ConfidenceLow || q.Source() != price.KindV3Spot {
		t.Errorf("dSTEALTH: %s via %s, want low via spot", q.Confidence, q.Source())
	}
}

// TestPriceLensClampsTWAPToAvailableHistory: asking for a longer window than the
// oracle holds returns the window it does hold, through the "buffer grown but not
// yet wrapped" path where slot index+1 is uninitialised and slot 0 is the oldest.
func TestPriceLensClampsTWAPToAvailableHistory(t *testing.T) {
	f := newPriceFixture(t)
	defer f.h.close()

	f.h.send(f.v3Pool, "setHistory", uint32(f.h.timestamp()-300), uint16(100))

	// The quote token rides along: Resolve crosses through what is in the snapshot,
	// which is why the service layer always appends the quote tokens itself.
	snap, err := price.Query(context.Background(), f.h.src(), price.Request{
		Tokens: []common.Address{f.stealth, f.weth}, Sources: f.sources, TWAPWindow: 1800,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var window uint32
	for _, p := range snap.Tokens[0].Pools {
		if p.Kind == price.PoolV3 {
			window = p.TWAPWindow
		}
	}
	// setHistory was mined in its own block, so the head is a second or so past it.
	if window < 300 || window > 310 {
		t.Errorf("twap window = %d, want ~300s (the history the pool holds), not the 1800 asked for", window)
	}

	res := price.Resolve(snap, f.sources, price.Policy{})
	if q := res.Quotes[f.stealth]; q.Source() != price.KindV3TWAP || q.Confidence != price.ConfidenceLow {
		t.Errorf("a five-minute TWAP is below the ten-minute policy floor: got %s via %s", q.Confidence, q.Source())
	}
}

// TestPriceLensSurvivesHostileSources points the factory and registry slots at
// contracts that are nothing of the kind. Every read is bounded, so the reply is
// simply empty where the source lied, and the call itself succeeds.
func TestPriceLensSurvivesHostileSources(t *testing.T) {
	f := newPriceFixture(t)
	defer f.h.close()

	bad := f.sources
	bad.FeedRegistry = f.usdc // an ERC-20 has no getFeed
	bad.V3Factory = f.dud     // nor getPool
	bad.V2Factory = f.v3Pool  // a v3 pool is not a v2 factory
	bad.NativeUSDFeed = f.stealth
	bad.Feeds = []price.FeedHint{{Token: f.stealth, Aggregator: f.v2Pair, Quote: price.QuoteUSD}}

	snap, err := price.Query(context.Background(), f.h.src(), price.Request{
		Tokens: []common.Address{f.stealth, f.usdc}, Sources: bad, TWAPWindow: 1800,
	})
	if err != nil {
		t.Fatalf("query against hostile sources: %v", err)
	}
	if snap.Native != nil && snap.Native.Answered {
		t.Errorf("an ERC-20 answered latestRoundData: %+v", snap.Native)
	}
	for _, tok := range snap.Tokens {
		if len(tok.Pools) != 0 {
			t.Errorf("%s found pools through non-factories: %+v", tok.Symbol, tok.Pools)
		}
		for _, fd := range tok.Feeds {
			if fd.Answered {
				t.Errorf("%s: a non-aggregator answered: %+v", tok.Symbol, fd)
			}
		}
	}
	res := price.Resolve(snap, bad, price.Policy{})
	if q := res.Quotes[f.stealth]; q.USD != nil {
		t.Errorf("hostile sources produced a price: %v", q.USD)
	}
}

// TestPricerCachesAcrossCalls: the service in front of the lens answers a repeat
// question from memory inside the TTL, and reads again once it expires.
func TestPricerCachesAcrossCalls(t *testing.T) {
	f := newPriceFixture(t)
	defer f.h.close()

	p := price.New(f.h.src(), price.Config{Sources: f.sources, CacheTTL: time.Minute}, nil)
	ctx := context.Background()

	first, err := p.Prices(ctx, []common.Address{f.stealth})
	if err != nil {
		t.Fatal(err)
	}
	if first.Calls != 1 || first.Cached != 0 {
		t.Errorf("first read: calls=%d cached=%d", first.Calls, first.Cached)
	}
	within(t, "dSTEALTH", first.Quotes[f.stealth].USD, 3, 1e-3)
	within(t, "native", first.Native.USD, 3000, 1e-9)

	second, err := p.Prices(ctx, []common.Address{f.stealth})
	if err != nil {
		t.Fatal(err)
	}
	if second.Calls != 0 || second.Cached != 1 {
		t.Errorf("second read: calls=%d cached=%d, want the cache to answer", second.Calls, second.Cached)
	}
	// A token the cache has not seen costs one call; the quote tokens ride along
	// and the already-cached one is not re-read.
	third, err := p.Prices(ctx, []common.Address{f.stealth, f.dud})
	if err != nil {
		t.Fatal(err)
	}
	if third.Calls != 1 || third.Cached != 1 {
		t.Errorf("third read: calls=%d cached=%d", third.Calls, third.Cached)
	}
	if third.Quotes[f.dud].USD != nil {
		t.Error("DUD has no market and must have no price")
	}
}
