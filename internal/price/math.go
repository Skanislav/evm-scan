package price

import (
	"math"
	"math/big"
	"strings"
)

// Everything here is exact rational arithmetic. A price that passed through a
// float64 on its way to a wallet has silently lost digits, and an 18-decimal token
// times a 1e-9 price is exactly the case where those digits matter.

var (
	bigOne = big.NewInt(1)
	// q96 is 2^96, the fixed-point scale of a Uniswap v3 sqrtPriceX96.
	q96  = new(big.Int).Lsh(bigOne, 96)
	q192 = new(big.Int).Lsh(bigOne, 192)
	// tickBase is 1.0001 with enough precision that 2^20 squarings of it still
	// carry ~200 correct bits; a tick never exceeds ±887272 in magnitude.
	tickBase, _ = new(big.Float).SetPrec(320).SetString("1.0001")
)

// ratioFromTick returns 1.0001^tick: the raw token1-per-token0 price at a tick.
func ratioFromTick(tick int32) *big.Rat {
	neg := tick < 0
	n := uint64(tick)
	if neg {
		n = uint64(-int64(tick))
	}
	result := new(big.Float).SetPrec(320).SetInt64(1)
	base := new(big.Float).SetPrec(320).Set(tickBase)
	for n > 0 {
		if n&1 == 1 {
			result.Mul(result, base)
		}
		base.Mul(base, base)
		n >>= 1
	}
	if neg {
		result.Quo(new(big.Float).SetPrec(320).SetInt64(1), result)
	}
	r, _ := result.Rat(nil)
	return r
}

// ratioFromSqrtPriceX96 returns (sqrtPriceX96 / 2^96)^2: the raw token1-per-token0
// spot price.
func ratioFromSqrtPriceX96(s *big.Int) *big.Rat {
	if s == nil || s.Sign() <= 0 {
		return nil
	}
	return new(big.Rat).SetFrac(new(big.Int).Mul(s, s), q192)
}

// twapTick is the arithmetic mean tick over a window, from the pool's cumulatives.
// Rounds toward negative infinity, as Uniswap's OracleLibrary does, so both sides
// agree on which tick a fractional average lands on.
func twapTick(start, end *big.Int, window uint32) (int32, bool) {
	if start == nil || end == nil || window == 0 {
		return 0, false
	}
	delta := new(big.Int).Sub(end, start)
	// big.Int.Div is Euclidean: for a positive divisor the quotient is the floor.
	q := new(big.Int).Div(delta, new(big.Int).SetUint64(uint64(window)))
	if !q.IsInt64() || q.Int64() > 887272 || q.Int64() < -887272 {
		return 0, false
	}
	return int32(q.Int64()), true
}

// wholeUnits rescales a raw token1-per-token0 ratio into whole tokens: one whole
// token0 is 10^dec0 raw units and buys ratio * 10^dec0 raw token1, which is
// ratio * 10^(dec0-dec1) whole token1.
func wholeUnits(raw *big.Rat, dec0, dec1 uint8) *big.Rat {
	out := new(big.Rat).Set(raw)
	if dec0 >= dec1 {
		return out.Mul(out, new(big.Rat).SetInt(pow10(uint(dec0-dec1))))
	}
	return out.Quo(out, new(big.Rat).SetInt(pow10(uint(dec1-dec0))))
}

// invert returns 1/r, or nil for zero.
func invert(r *big.Rat) *big.Rat {
	if r == nil || r.Sign() == 0 {
		return nil
	}
	return new(big.Rat).Inv(r)
}

// feedPrice scales a Chainlink answer by its decimals. Non-positive answers are not
// prices: Chainlink uses zero and negative values for "no data", and treating one
// as a price is how a portfolio gets valued at nothing.
func feedPrice(answer *big.Int, decimals uint8) *big.Rat {
	if answer == nil || answer.Sign() <= 0 {
		return nil
	}
	return new(big.Rat).SetFrac(answer, pow10(uint(decimals)))
}

// v3VirtualReserve is the in-range virtual reserve of one side of a v3 pool, in raw
// units: x = L / sqrtP for token0, y = L * sqrtP for token1. It is what the
// constant-product math sees at the current price, which makes it the honest
// counterpart to a v2 pair's reserve when comparing depth.
func v3VirtualReserve(liquidity, sqrtPriceX96 *big.Int, token0 bool) *big.Rat {
	if liquidity == nil || sqrtPriceX96 == nil || sqrtPriceX96.Sign() <= 0 {
		return nil
	}
	if token0 {
		return new(big.Rat).SetFrac(new(big.Int).Mul(liquidity, q96), sqrtPriceX96)
	}
	return new(big.Rat).SetFrac(new(big.Int).Mul(liquidity, sqrtPriceX96), q96)
}

func pow10(n uint) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(uint64(n)), nil)
}

// FormatPrice renders a USD price with enough places to be useful at its
// magnitude: four for anything over a dollar, more for the long tail, never in
// scientific notation and never through a float.
func FormatPrice(r *big.Rat) string {
	if r == nil {
		return ""
	}
	places := 4
	switch {
	case r.Cmp(big.NewRat(1, 100)) < 0:
		places = 12
	case r.Cmp(big.NewRat(1, 1)) < 0:
		places = 6
	}
	return trimZeros(r.FloatString(places), 2)
}

// FormatValue renders a USD amount to cents.
func FormatValue(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return r.FloatString(2)
}

// FormatRatio renders a token-in-token price (a hop), which can span many orders
// of magnitude, with a generous but bounded number of places.
func FormatRatio(r *big.Rat) string {
	if r == nil {
		return ""
	}
	return trimZeros(r.FloatString(18), 2)
}

// trimZeros drops trailing fractional zeros down to `keep` places.
func trimZeros(s string, keep int) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	end := len(s)
	for end > dot+1+keep && s[end-1] == '0' {
		end--
	}
	return s[:end]
}

// SqrtPriceX96FromTick is sqrt(1.0001^tick) * 2^96, the form a Uniswap v3 pool
// stores its price in. Exported for test and demo fixtures that need to seed a
// mock pool consistently; production only ever reads this value.
func SqrtPriceX96FromTick(tick int32) *big.Int {
	ratio := new(big.Float).SetPrec(320).SetRat(ratioFromTick(tick))
	sqrt := new(big.Float).SetPrec(320).Sqrt(ratio)
	sqrt.Mul(sqrt, new(big.Float).SetPrec(320).SetInt(q96))
	out, _ := sqrt.Int(nil)
	return out
}

// TickForRatio is the tick nearest a raw token1-per-token0 ratio. Fixture helper:
// float precision is plenty for choosing a tick to seed a mock with.
func TickForRatio(ratio float64) int32 {
	return int32(math.Round(math.Log(ratio) / math.Log(1.0001)))
}
