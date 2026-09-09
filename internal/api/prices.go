package api

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/price"
)

// Prices are read the same way balances are: from your own node, through a
// deployless contract, in one call. What the index contributes is nothing at all —
// a price is not a hint and is never committed on-chain. What the node contributes
// is every Chainlink feed and DEX pool it can find for the token, and the response
// says which of them it believed and why.

type hopJSON struct {
	Kind        string `json:"kind"`
	Address     string `json:"address,omitempty"`
	Pair        string `json:"pair"`
	Price       string `json:"price"`
	UpdatedAt   uint64 `json:"updated_at,omitempty"`
	AgeSeconds  *int64 `json:"age_seconds,omitempty"`
	Stale       bool   `json:"stale,omitempty"`
	Fee         uint32 `json:"fee,omitempty"`
	TWAPSeconds uint32 `json:"twap_seconds,omitempty"`
	// DepthUSD is the pool's quote-side reserve in USD: what it would cost to move
	// this price, which is the number to compare two pools by.
	DepthUSD string   `json:"depth_usd,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

type quoteJSON struct {
	// USD is the price of one whole token, as a decimal string. Absent when no
	// defensible source was found — never zero.
	USD        string `json:"usd,omitempty"`
	Confidence string `json:"confidence"`
	// Source is the kind of the route's first hop: chainlink, uniswap_v3_twap,
	// uniswap_v3_spot, uniswap_v2_spot, native_usd_feed or assumed_peg.
	Source    string    `json:"source,omitempty"`
	Stale     bool      `json:"stale,omitempty"`
	Route     []hopJSON `json:"route,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	AsOfBlock uint64    `json:"as_of_block"`
}

type priceCandidateJSON struct {
	USD        string    `json:"usd"`
	Confidence string    `json:"confidence"`
	Source     string    `json:"source"`
	Route      []hopJSON `json:"route"`
	Notes      []string  `json:"notes,omitempty"`
}

type feedSourceJSON struct {
	Aggregator      string `json:"aggregator"`
	Quote           string `json:"quote"`
	ViaRegistry     bool   `json:"via_registry,omitempty"`
	Answered        bool   `json:"answered"`
	Answer          string `json:"answer,omitempty"`
	Decimals        *uint8 `json:"decimals,omitempty"`
	UpdatedAt       uint64 `json:"updated_at,omitempty"`
	RoundID         string `json:"round_id,omitempty"`
	AnsweredInRound string `json:"answered_in_round,omitempty"`
	Description     string `json:"description,omitempty"`
}

type poolSourceJSON struct {
	Pool                string `json:"pool"`
	Kind                string `json:"kind"`
	Token0              string `json:"token0"`
	Token1              string `json:"token1"`
	Fee                 uint32 `json:"fee,omitempty"`
	SqrtPriceX96        string `json:"sqrt_price_x96,omitempty"`
	Tick                *int32 `json:"tick,omitempty"`
	Liquidity           string `json:"liquidity,omitempty"`
	TWAPSeconds         uint32 `json:"twap_seconds,omitempty"`
	TickCumulativeStart string `json:"tick_cumulative_start,omitempty"`
	TickCumulativeEnd   string `json:"tick_cumulative_end,omitempty"`
	Reserve0            string `json:"reserve0,omitempty"`
	Reserve1            string `json:"reserve1,omitempty"`
	ReserveTimestamp    uint32 `json:"reserve_timestamp,omitempty"`
}

type tokenPriceJSON struct {
	Address      string               `json:"address"`
	Symbol       string               `json:"symbol,omitempty"`
	Decimals     *uint8               `json:"decimals,omitempty"`
	Price        quoteJSON            `json:"price"`
	Alternatives []priceCandidateJSON `json:"alternatives,omitempty"`
	Feeds        []feedSourceJSON     `json:"feeds"`
	Pools        []poolSourceJSON     `json:"pools"`
}

func hopView(h price.Hop) hopJSON {
	v := hopJSON{
		Kind: h.Kind, Pair: h.Pair, Price: price.FormatRatio(h.Price),
		UpdatedAt: h.UpdatedAt, Stale: h.Stale, Fee: h.Fee, TWAPSeconds: h.TWAPWindow,
		DepthUSD: price.FormatValue(h.DepthUSD), Notes: h.Notes,
	}
	if h.Address != (common.Address{}) {
		v.Address = h.Address.Hex()
	}
	if h.UpdatedAt > 0 {
		age := int64(h.Age / time.Second)
		v.AgeSeconds = &age
	}
	return v
}

func routeView(route []price.Hop) []hopJSON {
	out := make([]hopJSON, len(route))
	for i, h := range route {
		out[i] = hopView(h)
	}
	return out
}

// quoteView renders a quote. A nil quote renders as "none", so callers can attach
// it unconditionally.
func quoteView(q *price.Quote) quoteJSON {
	if q == nil {
		return quoteJSON{Confidence: price.ConfidenceNone.String()}
	}
	return quoteJSON{
		USD:        price.FormatPrice(q.USD),
		Confidence: q.Confidence.String(),
		Source:     q.Source(),
		Stale:      q.Stale(),
		Route:      routeView(q.Route),
		Notes:      q.Notes,
		AsOfBlock:  q.AsOfBlock,
	}
}

func candidatesView(c []price.Candidate) []priceCandidateJSON {
	out := make([]priceCandidateJSON, len(c))
	for i, cand := range c {
		out[i] = priceCandidateJSON{
			USD: price.FormatPrice(cand.USD), Confidence: cand.Confidence.String(),
			Source: cand.Source(), Route: routeView(cand.Route), Notes: cand.Notes,
		}
	}
	return out
}

func sourcesView(t *price.TokenSources) (feeds []feedSourceJSON, pools []poolSourceJSON) {
	feeds = []feedSourceJSON{}
	pools = []poolSourceJSON{}
	if t == nil {
		return
	}
	for _, f := range t.Feeds {
		v := feedSourceJSON{
			Aggregator: f.Aggregator.Hex(), Quote: f.Quote.String(), ViaRegistry: f.ViaRegistry,
			Answered: f.Answered, Decimals: f.Decimals, UpdatedAt: f.UpdatedAt, Description: f.Description,
		}
		if f.Answer != nil {
			v.Answer = f.Answer.String()
		}
		if f.RoundID != nil {
			v.RoundID = f.RoundID.String()
		}
		if f.AnsweredInRound != nil {
			v.AnsweredInRound = f.AnsweredInRound.String()
		}
		feeds = append(feeds, v)
	}
	for _, p := range t.Pools {
		v := poolSourceJSON{
			Pool: p.Address.Hex(), Kind: p.Kind.String(), Token0: p.Token0.Hex(), Token1: p.Token1.Hex(), Fee: p.Fee,
		}
		switch p.Kind {
		case price.PoolV3:
			tick := p.Tick
			v.Tick = &tick
			v.SqrtPriceX96 = str(p.SqrtPriceX96)
			v.Liquidity = str(p.Liquidity)
			v.TWAPSeconds = p.TWAPWindow
			v.TickCumulativeStart = str(p.TickCumulativeStart)
			v.TickCumulativeEnd = str(p.TickCumulativeEnd)
		case price.PoolV2:
			v.Reserve0 = str(p.Reserve0)
			v.Reserve1 = str(p.Reserve1)
			v.ReserveTimestamp = p.ReserveTimestamp
		}
		pools = append(pools, v)
	}
	return
}

func str(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}

// pricerFor returns the chain's pricer, or nil when pricing is not configured.
func (s *Server) pricerFor(chainID uint64) *price.Pricer {
	p, ok := s.d.Pricers[chainID]
	if !ok || p == nil || !p.Enabled() {
		return nil
	}
	return p
}

// quotes prices a token list, tolerating failure: a portfolio without prices is
// still a portfolio, so a pricing error is logged and reported, never fatal.
func (s *Server) quotes(ctx context.Context, p *price.Pricer, tokens []common.Address) (*price.Result, string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := p.Prices(ctx, tokens)
	if err != nil {
		if s.d.Log != nil {
			s.d.Log.Warn("price read failed", "err", err)
		}
		return nil, err.Error()
	}
	return res, ""
}

// valuation multiplies a raw balance by a quote. Nil when either is unknown: an
// unpriced token contributes nothing to a total and says so, rather than zero.
func valuation(balance *big.Int, decimals *int16, q *price.Quote) *big.Rat {
	if balance == nil || q == nil || q.USD == nil {
		return nil
	}
	dec := uint8(18)
	switch {
	case decimals != nil && *decimals >= 0 && *decimals <= 77:
		dec = uint8(*decimals)
	case q.Decimals != nil:
		dec = *q.Decimals
	default:
		return nil
	}
	whole := new(big.Rat).SetFrac(balance, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
	return whole.Mul(whole, q.USD)
}

// totals accumulates a portfolio's USD value and the coverage behind it.
type totals struct {
	sum      *big.Rat
	priced   int
	unpriced int
	lowest   price.Confidence
}

func newTotals() *totals { return &totals{sum: new(big.Rat), lowest: price.ConfidenceHigh} }

func (t *totals) add(v *big.Rat, q *price.Quote) {
	if v == nil {
		t.unpriced++
		return
	}
	t.sum.Add(t.sum, v)
	t.priced++
	if q != nil && q.Confidence < t.lowest {
		t.lowest = q.Confidence
	}
}

func (t *totals) view(asOf uint64, errMsg string) map[string]any {
	out := map[string]any{
		"priced":   t.priced,
		"unpriced": t.unpriced,
	}
	if t.priced > 0 {
		out["total_usd"] = price.FormatValue(t.sum)
		// The total is only as good as its weakest price. A wallet showing one
		// number should show this next to it.
		out["confidence"] = t.lowest.String()
		out["as_of_block"] = asOf
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	return out
}

// listPrices is the discovery surface for prices: every source found for each
// token, the one chosen, and everything that lost to it.
func (s *Server) listPrices(w http.ResponseWriter, r *http.Request) {
	chainID, _, err := s.chainOf(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad chain", err)
		return
	}
	p := s.pricerFor(chainID)
	if p == nil {
		writeErr(w, http.StatusNotFound, "pricing not configured for this chain",
			errNoPricing(chainID))
		return
	}
	tokens, err := parseAddressList(r.URL.Query().Get("tokens"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad tokens", err)
		return
	}
	if len(tokens) > 256 {
		writeErr(w, http.StatusBadRequest, "too many tokens", nil)
		return
	}

	res, errMsg := s.quotes(r.Context(), p, tokens)
	if res == nil {
		writeErr(w, http.StatusBadGateway, "node read failed", errString(errMsg))
		return
	}

	out := make([]tokenPriceJSON, 0, len(tokens))
	for _, t := range tokens {
		q := res.Quotes[t]
		v := tokenPriceJSON{Address: t.Hex(), Price: quoteView(q)}
		if q != nil {
			v.Symbol, v.Decimals = q.Symbol, q.Decimals
			v.Alternatives = candidatesView(q.Alternatives)
		}
		v.Feeds, v.Pools = sourcesView(res.Sources[t])
		out = append(out, v)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":    chainID,
		"as_of_block": res.AsOfBlock,
		"native":      quoteView(res.Native),
		"tokens":      out,
		"calls":       res.Calls,
		"cached":      res.Cached,
		"sources":     sourcesConfigView(p),
		"read_by":     "deployless PriceLens (eth_call, no deployment); on-chain oracles and pools only, no quote API",
	})
}

// sourcesConfigView is where this chain's prices come from, so a consumer can check
// the factories and feeds against the deployments they trust.
func sourcesConfigView(p *price.Pricer) map[string]any {
	src := p.Sources()
	pol := p.Policy()
	quotes := make([]map[string]any, 0, len(src.QuoteTokens))
	for _, q := range src.QuoteTokens {
		quotes = append(quotes, map[string]any{
			"address": q.Address.Hex(), "symbol": q.Symbol,
			"wrapped_native": q.WrappedNative, "assume_usd_peg": q.AssumeUSDPeg,
		})
	}
	feeds := make([]map[string]any, 0, len(src.Feeds))
	for _, f := range src.Feeds {
		feeds = append(feeds, map[string]any{
			"token": f.Token.Hex(), "aggregator": f.Aggregator.Hex(), "quote": f.Quote.String(),
		})
	}
	return map[string]any{
		"feed_registry":      addrOrEmpty(src.FeedRegistry),
		"native_usd_feed":    addrOrEmpty(src.NativeUSDFeed),
		"pinned_feeds":       feeds,
		"uniswap_v3_factory": addrOrEmpty(src.V3Factory),
		"uniswap_v2_factory": addrOrEmpty(src.V2Factory),
		"fee_tiers":          src.FeeTiers,
		"quote_tokens":       quotes,
		"twap_window":        p.TWAPWindow().String(),
		"min_twap_window":    pol.MinTWAPWindow.String(),
		"max_feed_age":       pol.MaxFeedAge.String(),
	}
}

func addrOrEmpty(a common.Address) string {
	if a == (common.Address{}) {
		return ""
	}
	return a.Hex()
}

type errString string

func (e errString) Error() string { return string(e) }

func errNoPricing(chainID uint64) error {
	return errString("set chains[].pricing for chain " + itoa(chainID) +
		", or index a chain with built-in defaults (1, 10, 8453, 42161)")
}

func itoa(v uint64) string { return new(big.Int).SetUint64(v).String() }
