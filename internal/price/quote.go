package price

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Confidence is how much a consumer should lean on a price.
//
// Three tiers rather than a score, because a score invites false precision about
// exactly the thing this cannot measure: whether a number can be moved by someone
// who wants your wallet to show the wrong total.
type Confidence uint8

const (
	// ConfidenceNone means no defensible source was found. USD is nil.
	ConfidenceNone Confidence = iota
	// ConfidenceLow is a spot price, a stale feed, or a route through either. It
	// can be moved inside one block by anyone with enough capital.
	ConfidenceLow
	// ConfidenceMedium is a time-weighted DEX price over at least the minimum
	// window, crossed into USD through a fresh feed or an assumed peg.
	ConfidenceMedium
	// ConfidenceHigh is a fresh Chainlink feed, directly in USD or crossed through
	// the fresh native/USD feed.
	ConfidenceHigh
)

func (c Confidence) String() string {
	switch c {
	case ConfidenceHigh:
		return "high"
	case ConfidenceMedium:
		return "medium"
	case ConfidenceLow:
		return "low"
	default:
		return "none"
	}
}

// Kinds of hop a route is made of.
const (
	KindChainlink  = "chainlink"
	KindNativeFeed = "native_usd_feed"
	KindV3TWAP     = "uniswap_v3_twap"
	KindV3Spot     = "uniswap_v3_spot"
	KindV2Spot     = "uniswap_v2_spot"
	KindAssumedPeg = "assumed_peg"
)

// Policy is what separates a price from a number.
type Policy struct {
	// MaxFeedAge is how old a Chainlink round may be before it is stale. Feeds
	// have heartbeats from an hour to a day, so this is generous by default; an
	// operator who knows their feeds can tighten it.
	MaxFeedAge time.Duration
	// MinTWAPWindow is the shortest averaging window that still counts as a
	// TWAP rather than a spot. Below it a pool price is reported at low confidence.
	MinTWAPWindow time.Duration
}

// DefaultPolicy is what a deployment gets unless it says otherwise.
var DefaultPolicy = Policy{
	MaxFeedAge:    25 * time.Hour,
	MinTWAPWindow: 10 * time.Minute,
}

func (p Policy) withDefaults() Policy {
	if p.MaxFeedAge <= 0 {
		p.MaxFeedAge = DefaultPolicy.MaxFeedAge
	}
	if p.MinTWAPWindow <= 0 {
		p.MinTWAPWindow = DefaultPolicy.MinTWAPWindow
	}
	return p
}

// Hop is one step of a route: a feed read, a pool observation, or an assumption.
type Hop struct {
	Kind string
	// Address is the aggregator or pool. Zero for an assumption.
	Address common.Address
	// Pair names what the hop prices in what, e.g. "STEALTH/WETH" or "ETH / USD".
	Pair string
	// Price is one unit of the hop's base in its quote.
	Price *big.Rat
	// Feed-specific.
	UpdatedAt uint64
	Age       time.Duration
	Stale     bool
	// Pool-specific.
	Fee        uint32
	TWAPWindow uint32
	// DepthUSD is the quote-side reserve of the pool valued in USD: for a v2 pair
	// the real reserve, for v3 the in-range virtual reserve. It is the cost of
	// moving the price, which is what a consumer comparing two pools wants.
	DepthUSD *big.Rat
	Notes    []string
}

// Candidate is one way the token could be priced.
type Candidate struct {
	USD        *big.Rat
	Confidence Confidence
	Route      []Hop
	Notes      []string

	// ordering within a confidence tier
	feed  bool
	age   time.Duration
	depth *big.Rat
}

// Source is a one-word summary of how a candidate was priced.
func (c Candidate) Source() string {
	if len(c.Route) == 0 {
		return ""
	}
	return c.Route[0].Kind
}

// Quote is the price chosen for a token, with everything that lost to it.
type Quote struct {
	Token      common.Address
	Symbol     string
	Decimals   *uint8
	USD        *big.Rat
	Confidence Confidence
	Route      []Hop
	Notes      []string
	// Alternatives is every candidate, best first; Route is Alternatives[0].Route.
	Alternatives []Candidate
	AsOfBlock    uint64
	Timestamp    uint64
}

// Source is the kind of the chosen route's first hop, or "" for no price.
func (q *Quote) Source() string {
	if q == nil || len(q.Route) == 0 {
		return ""
	}
	return q.Route[0].Kind
}

// Stale reports that the chosen route went through a stale feed.
func (q *Quote) Stale() bool {
	if q == nil {
		return false
	}
	for _, h := range q.Route {
		if h.Stale {
			return true
		}
	}
	return false
}

// Resolution is what Resolve makes of a snapshot.
type Resolution struct {
	// Native is the native asset's quote, or nil when no feed answered.
	Native *Quote
	Quotes map[common.Address]*Quote
}

// Resolve turns a snapshot into prices under a policy.
//
// Quote tokens are priced from their own feeds only, never through pools, so a route
// is at most token -> quote token -> USD and can never loop.
func Resolve(snap *Snapshot, sources Sources, policy Policy) Resolution {
	policy = policy.withDefaults()
	r := resolver{
		snap:    snap,
		sources: sources,
		policy:  policy,
		now:     time.Unix(int64(snap.Timestamp), 0),
		byAddr:  map[common.Address]*TokenSources{},
		quotes:  map[common.Address]QuoteToken{},
	}
	for i := range snap.Tokens {
		r.byAddr[snap.Tokens[i].Token] = &snap.Tokens[i]
	}
	for _, q := range sources.QuoteTokens {
		r.quotes[q.Address] = q
	}

	out := Resolution{Quotes: map[common.Address]*Quote{}}
	out.Native = r.native()
	for i := range snap.Tokens {
		t := &snap.Tokens[i]
		out.Quotes[t.Token] = r.token(t)
	}
	return out
}

type resolver struct {
	snap    *Snapshot
	sources Sources
	policy  Policy
	now     time.Time
	byAddr  map[common.Address]*TokenSources
	quotes  map[common.Address]QuoteToken
}

// native prices the chain's own asset off its USD feed.
func (r *resolver) native() *Quote {
	q := &Quote{AsOfBlock: r.snap.BlockNumber, Timestamp: r.snap.Timestamp, Symbol: "native"}
	if hop, conf, ok := r.feedHop(r.snap.Native, "native / USD"); ok {
		hop.Kind = KindNativeFeed
		c := Candidate{USD: hop.Price, Confidence: conf, Route: []Hop{hop}, feed: true, age: hop.Age}
		q.Alternatives = []Candidate{c}
		q.USD, q.Confidence, q.Route, q.Notes = c.USD, c.Confidence, c.Route, c.Notes
		return q
	}
	q.Notes = []string{"no native/USD feed answered"}
	return q
}

func (r *resolver) token(t *TokenSources) *Quote {
	q := &Quote{
		Token: t.Token, Symbol: t.Symbol, Decimals: t.Decimals,
		AsOfBlock: r.snap.BlockNumber, Timestamp: r.snap.Timestamp,
	}
	if !t.IsContract {
		q.Notes = []string{"address has no code"}
		return q
	}

	var cands []Candidate
	cands = append(cands, r.feedCandidates(t)...)
	cands = append(cands, r.quoteTokenCandidates(t)...)
	if t.Decimals != nil {
		cands = append(cands, r.poolCandidates(t)...)
	} else if len(t.Pools) > 0 {
		q.Notes = append(q.Notes, "token did not report decimals, so its pools cannot be priced")
	}

	rank(cands)
	q.Alternatives = cands
	if len(cands) == 0 {
		if len(q.Notes) == 0 {
			q.Notes = []string{"no price source found on-chain"}
		}
		return q
	}
	best := cands[0]
	q.USD, q.Confidence, q.Route = best.USD, best.Confidence, best.Route
	q.Notes = append(q.Notes, best.Notes...)
	return q
}

// feedCandidates: the token's own Chainlink feeds, in USD directly or crossed
// through the native feed.
func (r *resolver) feedCandidates(t *TokenSources) []Candidate {
	var out []Candidate
	for i := range t.Feeds {
		f := &t.Feeds[i]
		pair := f.Description
		if pair == "" {
			pair = fmt.Sprintf("%s / %s", symbolOr(t), f.Quote)
		}
		hop, conf, ok := r.feedHop(f, pair)
		if !ok {
			continue
		}
		switch f.Quote {
		case QuoteUSD:
			out = append(out, Candidate{
				USD: hop.Price, Confidence: conf, Route: []Hop{hop},
				Notes: hop.Notes, feed: true, age: hop.Age,
			})
		case QuoteNative:
			nHop, nConf, ok := r.feedHop(r.snap.Native, "native / USD")
			if !ok {
				continue
			}
			nHop.Kind = KindNativeFeed
			usd := new(big.Rat).Mul(hop.Price, nHop.Price)
			out = append(out, Candidate{
				USD: usd, Confidence: minConf(conf, nConf), Route: []Hop{hop, nHop},
				Notes: append(append([]string{}, hop.Notes...), nHop.Notes...),
				feed:  true, age: max(hop.Age, nHop.Age),
			})
		}
	}
	return out
}

// quoteTokenCandidates handles a token that is itself a configured quote token:
// wrapped native is priced off the native feed, a stable without a feed off its peg.
func (r *resolver) quoteTokenCandidates(t *TokenSources) []Candidate {
	qt, ok := r.quotes[t.Token]
	if !ok {
		return nil
	}
	var out []Candidate
	if qt.WrappedNative {
		if hop, conf, ok := r.feedHop(r.snap.Native, "native / USD"); ok {
			hop.Kind = KindNativeFeed
			hop.Notes = append(hop.Notes, fmt.Sprintf("%s priced as the wrapped native asset", symbolOr(t)))
			out = append(out, Candidate{USD: hop.Price, Confidence: conf, Route: []Hop{hop}, Notes: hop.Notes, feed: true, age: hop.Age})
		}
	}
	if qt.AssumeUSDPeg {
		hop := Hop{Kind: KindAssumedPeg, Pair: symbolOr(t) + " / USD", Price: big.NewRat(1, 1),
			Notes: []string{fmt.Sprintf("%s assumed to be exactly 1 USD; no feed observed it", symbolOr(t))}}
		out = append(out, Candidate{USD: hop.Price, Confidence: ConfidenceLow, Route: []Hop{hop}, Notes: hop.Notes})
	}
	return out
}

// poolCandidates: every pool the lens found, crossed into USD through the quote
// token on its other side.
func (r *resolver) poolCandidates(t *TokenSources) []Candidate {
	var out []Candidate
	for i := range t.Pools {
		p := &t.Pools[i]
		var quoteAddr common.Address
		tokenIs0 := false
		switch t.Token {
		case p.Token0:
			quoteAddr, tokenIs0 = p.Token1, true
		case p.Token1:
			quoteAddr = p.Token0
		default:
			continue
		}
		quote := r.byAddr[quoteAddr]
		if quote == nil || quote.Decimals == nil {
			continue
		}
		qUSD, qConf, qHop, ok := r.quoteUSD(quote)
		if !ok {
			continue
		}

		hop, conf, ok := r.poolHop(p, t, quote, tokenIs0)
		if !ok {
			continue
		}
		depth := r.depthUSD(p, quote, !tokenIs0, qUSD)
		hop.DepthUSD = depth

		usd := new(big.Rat).Mul(hop.Price, qUSD)
		c := Candidate{
			USD:        usd,
			Confidence: minConf(conf, qConf),
			Route:      []Hop{hop, qHop},
			Notes:      append(append([]string{}, hop.Notes...), qHop.Notes...),
			depth:      depth,
		}
		out = append(out, c)
	}
	return out
}

// quoteUSD prices a quote token from its own feeds only — no pools, so routes
// cannot chain through each other.
func (r *resolver) quoteUSD(q *TokenSources) (*big.Rat, Confidence, Hop, bool) {
	var cands []Candidate
	cands = append(cands, r.feedCandidates(q)...)
	cands = append(cands, r.quoteTokenCandidates(q)...)
	if len(cands) == 0 {
		return nil, ConfidenceNone, Hop{}, false
	}
	rank(cands)
	best := cands[0]
	// Collapse a two-hop quote route (TOKEN/ETH × ETH/USD) into its first hop for
	// display; the confidence already accounts for both legs.
	hop := best.Route[0]
	if len(best.Route) > 1 {
		hop.Price = best.USD
		hop.Pair = fmt.Sprintf("%s / USD (via %s)", symbolOr(q), best.Route[1].Pair)
		hop.Stale = hop.Stale || best.Route[1].Stale
	}
	// An assumed peg is fine for crossing, but caps what it can vouch for.
	conf := best.Confidence
	if hop.Kind == KindAssumedPeg {
		conf = ConfidenceMedium
	}
	return best.USD, conf, hop, true
}

// feedHop reads one feed under the staleness policy.
func (r *resolver) feedHop(f *Feed, pair string) (Hop, Confidence, bool) {
	if f == nil || !f.Answered || f.Decimals == nil {
		return Hop{}, ConfidenceNone, false
	}
	price := feedPrice(f.Answer, *f.Decimals)
	if price == nil {
		return Hop{}, ConfidenceNone, false
	}
	hop := Hop{
		Kind:      KindChainlink,
		Address:   f.Aggregator,
		Pair:      pair,
		Price:     price,
		UpdatedAt: f.UpdatedAt,
	}
	if f.UpdatedAt > 0 {
		hop.Age = r.now.Sub(time.Unix(int64(f.UpdatedAt), 0))
		if hop.Age < 0 {
			hop.Age = 0
		}
	}
	conf := ConfidenceHigh
	if f.UpdatedAt == 0 || hop.Age > r.policy.MaxFeedAge {
		hop.Stale = true
		conf = ConfidenceLow
		hop.Notes = append(hop.Notes, fmt.Sprintf("feed %s is stale: last update %s ago", pair, hop.Age.Round(time.Second)))
	}
	if f.AnsweredInRound != nil && f.RoundID != nil && f.AnsweredInRound.Cmp(f.RoundID) < 0 {
		hop.Stale = true
		conf = ConfidenceLow
		hop.Notes = append(hop.Notes, fmt.Sprintf("feed %s answered in an earlier round than it reports", pair))
	}
	return hop, conf, true
}

// poolHop prices the token in the quote token from one pool.
func (r *resolver) poolHop(p *Pool, t, quote *TokenSources, tokenIs0 bool) (Hop, Confidence, bool) {
	hop := Hop{Address: p.Address, Fee: p.Fee, Pair: fmt.Sprintf("%s/%s", symbolOr(t), symbolOr(quote))}
	dec0, dec1 := *t.Decimals, *quote.Decimals
	if !tokenIs0 {
		dec0, dec1 = dec1, dec0
	}

	var raw *big.Rat
	conf := ConfidenceLow
	switch p.Kind {
	case PoolV3:
		if p.TWAPWindow > 0 {
			tick, ok := twapTick(p.TickCumulativeStart, p.TickCumulativeEnd, p.TWAPWindow)
			if !ok {
				return Hop{}, ConfidenceNone, false
			}
			raw = ratioFromTick(tick)
			hop.Kind = KindV3TWAP
			hop.TWAPWindow = p.TWAPWindow
			window := time.Duration(p.TWAPWindow) * time.Second
			if window >= r.policy.MinTWAPWindow {
				conf = ConfidenceMedium
			} else {
				hop.Notes = append(hop.Notes, fmt.Sprintf("pool oracle only covered %s, below the %s minimum",
					window, r.policy.MinTWAPWindow))
			}
		} else {
			raw = ratioFromSqrtPriceX96(p.SqrtPriceX96)
			hop.Kind = KindV3Spot
			hop.Notes = append(hop.Notes, "spot price: the pool's oracle held no history to average; movable within one block")
		}
	case PoolV2:
		if p.Reserve0 == nil || p.Reserve1 == nil || p.Reserve0.Sign() == 0 || p.Reserve1.Sign() == 0 {
			return Hop{}, ConfidenceNone, false
		}
		raw = new(big.Rat).SetFrac(p.Reserve1, p.Reserve0)
		hop.Kind = KindV2Spot
		hop.Notes = append(hop.Notes, "spot price from reserves: movable within one block")
	default:
		return Hop{}, ConfidenceNone, false
	}
	if raw == nil || raw.Sign() <= 0 {
		return Hop{}, ConfidenceNone, false
	}

	// raw is token1 per token0 in smallest units; the hop wants token in quote,
	// in whole units.
	price := wholeUnits(raw, dec0, dec1)
	if !tokenIs0 {
		price = invert(price)
		if price == nil {
			return Hop{}, ConfidenceNone, false
		}
	}
	hop.Price = price
	return hop, conf, true
}

// depthUSD values the quote side of a pool in USD.
func (r *resolver) depthUSD(p *Pool, quote *TokenSources, quoteIs0 bool, quoteUSD *big.Rat) *big.Rat {
	var reserve *big.Rat
	switch p.Kind {
	case PoolV3:
		reserve = v3VirtualReserve(p.Liquidity, p.SqrtPriceX96, quoteIs0)
	case PoolV2:
		res := p.Reserve1
		if quoteIs0 {
			res = p.Reserve0
		}
		if res != nil {
			reserve = new(big.Rat).SetInt(res)
		}
	}
	if reserve == nil {
		return nil
	}
	whole := new(big.Rat).Quo(reserve, new(big.Rat).SetInt(pow10(uint(*quote.Decimals))))
	return whole.Mul(whole, quoteUSD)
}

// rank orders candidates best first: confidence, then feeds before pools, then
// the freshest feed or the deepest pool.
func rank(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool {
		a, b := c[i], c[j]
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.feed != b.feed {
			return a.feed
		}
		if a.feed {
			return a.age < b.age
		}
		switch {
		case a.depth == nil && b.depth == nil:
			return false
		case a.depth == nil:
			return false
		case b.depth == nil:
			return true
		}
		return a.depth.Cmp(b.depth) > 0
	})
}

func minConf(a, b Confidence) Confidence {
	if a < b {
		return a
	}
	return b
}

func symbolOr(t *TokenSources) string {
	if t.Symbol != "" {
		return t.Symbol
	}
	return t.Token.Hex()[:10]
}
