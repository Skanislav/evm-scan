package price

import (
	"math"
	"math/big"
	"testing"
)

func ratF(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}

func TestRatioFromTick(t *testing.T) {
	cases := []struct {
		tick int32
		want float64
	}{
		{0, 1},
		{1, 1.0001},
		{-1, 1 / 1.0001},
		{6931, math.Pow(1.0001, 6931)},
		{-6931, math.Pow(1.0001, -6931)},
		// USDC/WETH on mainnet sits around here: 1 raw USDC buys ~3.3e8 raw WETH.
		{196000, math.Pow(1.0001, 196000)},
	}
	// float64's Pow drifts by ~exponent*epsilon, so the reference is only good to
	// about 1e-10 at large ticks; the exact check below uses Uniswap's constants.
	for _, c := range cases {
		got := ratF(ratioFromTick(c.tick))
		if rel := math.Abs(got-c.want) / c.want; rel > 1e-9 {
			t.Errorf("tick %d: got %g want %g (rel %g)", c.tick, got, c.want, rel)
		}
	}
}

// TestRatioFromTickMatchesUniswapConstants pins the two ends of the tick range to
// TickMath.MIN_SQRT_RATIO and MAX_SQRT_RATIO, which are the exact values every v3
// pool on every chain computes with.
func TestRatioFromTickMatchesUniswapConstants(t *testing.T) {
	minSqrt, _ := new(big.Int).SetString("4295128739", 10)
	maxSqrt, _ := new(big.Int).SetString("1461446703485210103287273052203988822378723970342", 10)
	for _, c := range []struct {
		tick int32
		sqrt *big.Int
	}{{-887272, minSqrt}, {887272, maxSqrt}, {0, q96}} {
		want := new(big.Float).SetPrec(256).SetRat(ratioFromSqrtPriceX96(c.sqrt))
		got := new(big.Float).SetPrec(256).SetRat(ratioFromTick(c.tick))
		rel := new(big.Float).Quo(new(big.Float).Sub(got, want), want)
		if rel.Abs(rel).Cmp(big.NewFloat(1e-9)) > 0 {
			t.Errorf("tick %d: ratio %s, TickMath says %s (rel %s)", c.tick, got.Text('g', 20), want.Text('g', 20), rel.Text('g', 3))
		}
	}
}

// TestTickAndSqrtPriceAgree pins the two spot representations to each other: a
// tick's ratio and the sqrtPriceX96 at that tick must describe the same price.
func TestTickAndSqrtPriceAgree(t *testing.T) {
	for _, tick := range []int32{0, 100, -100, 50_000, -200_000} {
		// sqrtPriceX96 = sqrt(1.0001^tick) * 2^96, built from the ratio itself.
		ratio := ratioFromTick(tick)
		sqrt := new(big.Float).SetPrec(256).Sqrt(new(big.Float).SetPrec(256).SetRat(ratio))
		sqrt.Mul(sqrt, new(big.Float).SetInt(q96))
		sqrtX96, _ := sqrt.Int(nil)

		got := ratF(ratioFromSqrtPriceX96(sqrtX96))
		want := ratF(ratio)
		if rel := math.Abs(got-want) / want; rel > 1e-9 {
			t.Errorf("tick %d: sqrt path %g, tick path %g", tick, got, want)
		}
	}
}

func TestTWAPTickFloors(t *testing.T) {
	cases := []struct {
		start, end int64
		window     uint32
		want       int32
	}{
		{0, 6000, 600, 10},
		{0, -6000, 600, -10},
		// Uniswap rounds a fractional negative mean toward negative infinity.
		{0, -6001, 600, -11},
		{0, 6001, 600, 10},
		{1_000_000, 1_000_000, 60, 0},
	}
	for _, c := range cases {
		got, ok := twapTick(big.NewInt(c.start), big.NewInt(c.end), c.window)
		if !ok || got != c.want {
			t.Errorf("twap(%d,%d,%d) = %d ok=%v, want %d", c.start, c.end, c.window, got, ok, c.want)
		}
	}
	if _, ok := twapTick(big.NewInt(0), big.NewInt(1), 0); ok {
		t.Error("zero window must not produce a tick")
	}
}

func TestWholeUnits(t *testing.T) {
	// A USDC(6)/WETH(18) pool at tick 196000: raw ratio ~3.3e8 raw WETH per raw
	// USDC becomes ~3.3e-4 WETH per whole USDC.
	raw := ratioFromTick(196000)
	got := ratF(wholeUnits(raw, 6, 18))
	want := math.Pow(1.0001, 196000) * 1e-12
	if rel := math.Abs(got-want) / want; rel > 1e-9 {
		t.Errorf("wholeUnits = %g want %g", got, want)
	}
	// And the inverse is ETH in USDC, around 3000.
	usdc := ratF(invert(wholeUnits(raw, 6, 18)))
	if usdc < 2000 || usdc > 4000 {
		t.Errorf("1 WETH = %g USDC at tick 196000; expected a few thousand", usdc)
	}
	// Equal decimals is the identity.
	if wholeUnits(big.NewRat(3, 2), 18, 18).Cmp(big.NewRat(3, 2)) != 0 {
		t.Error("equal decimals changed the ratio")
	}
}

func TestFeedPrice(t *testing.T) {
	if got := feedPrice(big.NewInt(301234000000), 8); got.FloatString(4) != "3012.3400" {
		t.Errorf("feedPrice = %s", got.FloatString(4))
	}
	if feedPrice(big.NewInt(0), 8) != nil || feedPrice(big.NewInt(-1), 8) != nil || feedPrice(nil, 8) != nil {
		t.Error("non-positive answers must not become prices")
	}
}

func TestV3VirtualReserve(t *testing.T) {
	// At sqrtP = 2^96 (price 1) both virtual reserves equal L.
	l := big.NewInt(1_000_000)
	if r := v3VirtualReserve(l, q96, true); r.Cmp(new(big.Rat).SetInt(l)) != 0 {
		t.Errorf("token0 reserve at price 1 = %s", r.FloatString(2))
	}
	if r := v3VirtualReserve(l, q96, false); r.Cmp(new(big.Rat).SetInt(l)) != 0 {
		t.Errorf("token1 reserve at price 1 = %s", r.FloatString(2))
	}
	// At sqrtP = 2 * 2^96 (price 4): x = L/2, y = 2L.
	twice := new(big.Int).Lsh(q96, 1)
	if r := v3VirtualReserve(l, twice, true); r.Cmp(big.NewRat(500_000, 1)) != 0 {
		t.Errorf("token0 reserve at price 4 = %s", r.FloatString(2))
	}
	if r := v3VirtualReserve(l, twice, false); r.Cmp(big.NewRat(2_000_000, 1)) != 0 {
		t.Errorf("token1 reserve at price 4 = %s", r.FloatString(2))
	}
}

func TestFormatting(t *testing.T) {
	cases := []struct {
		in   *big.Rat
		want string
	}{
		{big.NewRat(3012_4471, 10_000), "3012.4471"},
		{big.NewRat(1, 1), "1.00"},
		{big.NewRat(1, 2), "0.50"},
		{big.NewRat(123456, 1_000_000), "0.123456"},
		{big.NewRat(1, 1_000_000_000), "0.000000001"},
		{big.NewRat(1, 10_000_000_000_000), "0.00"}, // below 12 places: rounds away, never scientific
	}
	for _, c := range cases {
		if got := FormatPrice(c.in); got != c.want {
			t.Errorf("FormatPrice(%s) = %q want %q", c.in, got, c.want)
		}
	}
	if got := FormatValue(big.NewRat(1234567, 100)); got != "12345.67" {
		t.Errorf("FormatValue = %q", got)
	}
	if FormatPrice(nil) != "" || FormatValue(nil) != "" {
		t.Error("nil formats to empty")
	}
}
