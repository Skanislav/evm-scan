// Package price discovers and reads on-chain price sources through a contract that
// is never deployed, then turns what it found into a USD price it can defend.
//
// # Why prices belong here at all
//
// evm-scan's premise is that everything a wallet needs is public state on your own
// node. Prices look like the exception — the obvious way to get one is a quote API,
// which is a third party in the path again — but they are not: Chainlink
// aggregators and DEX pools are contracts, readable at the head by a snap-synced
// node. The hard part is *finding* them for a token nobody configured, and that is
// what this package does on-chain, in one call:
//
//   - the Chainlink Feed Registry, where the chain has one, for TOKEN/USD and TOKEN/ETH;
//   - aggregators the operator pinned explicitly;
//   - every Uniswap v3 pool between the token and each quote token, with the pool's
//     own TWAP oracle read over the requested window;
//   - every Uniswap v2 pair between the token and each quote token.
//
// contracts/src/PriceLens.sol returns all of it, raw. Resolve turns it into a price
// with a stated source and confidence: a fresh feed beats a TWAP beats a spot, a
// spot is labelled as the manipulable number it is, and a token with no defensible
// source gets no price rather than a made-up one.
//
// # Deployless, like internal/lens
//
// The same eth_call-with-no-`to` trick, the same EIP-170/EIP-3860 limits, the same
// batching that halves when a node says a reply is too large.
package price

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/token"
)

const (
	// MaxReplyBytes is EIP-170's code-size ceiling on the constructor's reply.
	MaxReplyBytes = 24576
	// MaxPayloadBytes is EIP-3860's initcode ceiling on the request.
	MaxPayloadBytes = 49152
	// DefaultTokensPerCall is deliberately small: a major token can have a dozen
	// pools, and each pool is ~15 words of reply.
	DefaultTokensPerCall = 8
)

// QuoteKind is what a Chainlink answer is denominated in.
type QuoteKind uint8

const (
	QuoteUSD    QuoteKind = 0
	QuoteNative QuoteKind = 1
)

func (q QuoteKind) String() string {
	if q == QuoteNative {
		return "native"
	}
	return "usd"
}

// PoolKind is the DEX shape a pool was read as.
type PoolKind uint8

const (
	PoolV2 PoolKind = 2
	PoolV3 PoolKind = 3
)

func (k PoolKind) String() string {
	switch k {
	case PoolV2:
		return "uniswap_v2"
	case PoolV3:
		return "uniswap_v3"
	default:
		return "unknown"
	}
}

// FeedHint pins a Chainlink aggregator to a token, for chains without a registry.
type FeedHint struct {
	Token      common.Address
	Aggregator common.Address
	Quote      QuoteKind
}

// Sources is where one chain's prices can be found. Zero values mean "none".
type Sources struct {
	FeedRegistry  common.Address
	NativeUSDFeed common.Address
	Feeds         []FeedHint
	V3Factory     common.Address
	FeeTiers      []uint32
	V2Factory     common.Address
	// QuoteTokens are the pool counterparties: the wrapped native asset and the
	// major stables. Order is preference order when everything else ties.
	QuoteTokens []QuoteToken
}

// QuoteToken is a pool counterparty and how its own USD price is known.
type QuoteToken struct {
	Address common.Address
	Symbol  string
	// WrappedNative prices the token off the native/USD feed: the registry keys
	// ETH by a sentinel, not by WETH's address, so WETH would otherwise have no feed.
	WrappedNative bool
	// AssumeUSDPeg treats the token as exactly 1 USD when no feed covers it. Only
	// ever a fallback, and every price routed through it says so.
	AssumeUSDPeg bool
}

// Empty reports that no source is configured at all.
func (s Sources) Empty() bool {
	return s.FeedRegistry == (common.Address{}) && s.NativeUSDFeed == (common.Address{}) &&
		len(s.Feeds) == 0 && s.V3Factory == (common.Address{}) && s.V2Factory == (common.Address{})
}

func (s Sources) quoteAddresses() []common.Address {
	out := make([]common.Address, len(s.QuoteTokens))
	for i, q := range s.QuoteTokens {
		out[i] = q.Address
	}
	return out
}

// Request is one lens call's worth of questions.
type Request struct {
	Tokens  []common.Address
	Sources Sources
	// TWAPWindow is the averaging window asked of each v3 pool, in seconds. The
	// pool's oracle may hold less history; each pool reports what it gave.
	TWAPWindow uint32
	// GasPerCall caps each staticcall the lens makes. Zero uses the contract's default.
	GasPerCall uint64
	// MaxStringBytes truncates every returned string. Zero uses the contract's default.
	MaxStringBytes uint64
	// TokensPerCall bounds one eth_call. Zero uses DefaultTokensPerCall.
	TokensPerCall int
}

// Feed is one Chainlink-shaped read.
type Feed struct {
	Aggregator  common.Address
	Quote       QuoteKind
	ViaRegistry bool
	// Answered reports that latestRoundData came back well-formed. Answer is only
	// meaningful when it did, and even then a non-positive answer is not a price.
	Answered        bool
	Answer          *big.Int
	Decimals        *uint8
	StartedAt       uint64
	UpdatedAt       uint64
	RoundID         *big.Int
	AnsweredInRound *big.Int
	Description     string
}

// Pool is one DEX pool between the token and a quote token, raw.
type Pool struct {
	Address common.Address
	Kind    PoolKind
	Token0  common.Address
	Token1  common.Address
	Fee     uint32
	// v3
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
	// TWAPWindow is the seconds actually averaged over; zero means the oracle had
	// no history and only the spot is available.
	TWAPWindow          uint32
	TickCumulativeStart *big.Int
	TickCumulativeEnd   *big.Int
	// v2
	Reserve0         *big.Int
	Reserve1         *big.Int
	ReserveTimestamp uint32
}

// TokenSources is everything the lens found for one token.
type TokenSources struct {
	Token      common.Address
	IsContract bool
	Symbol     string
	Decimals   *uint8
	Feeds      []Feed
	Pools      []Pool
}

// Snapshot is what the chain said about every requested token, as of one block.
type Snapshot struct {
	ChainID     uint64
	BlockNumber uint64
	ParentHash  common.Hash
	Timestamp   uint64
	// Native is the native asset's USD feed, or nil when none was found.
	Native *Feed
	Tokens []TokenSources
	Calls  int
	// Atomic is false when batching split the read and the head moved between
	// calls; every token is still from a real block, just not the same one.
	Atomic bool
}

// Query reads the request against head state, batching tokens across as few
// eth_calls as the reply-size limit allows.
func Query(ctx context.Context, src chain.Source, req Request) (*Snapshot, error) {
	batch := req.TokensPerCall
	if batch <= 0 {
		batch = DefaultTokensPerCall
	}

	var (
		out   *Snapshot
		first wireChainInfo
		calls int
	)
	merged := make([]TokenSources, 0, len(req.Tokens))
	for i := 0; i < len(req.Tokens) || i == 0; {
		n := min(batch, len(req.Tokens)-i)
		w, err := callOnce(ctx, src, req, req.Tokens[i:i+n])
		calls++
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if tooBig(err) && n > 1 {
				batch = max(1, n/2)
				continue
			}
			return nil, fmt.Errorf("price: query %d token(s) from index %d: %w", n, i, err)
		}

		if out == nil {
			out = w.view()
			first = w.Chain
		} else if w.Chain.BlockNumber.Cmp(first.BlockNumber) != 0 || w.Chain.ParentHash != first.ParentHash {
			out.Atomic = false
		}
		for _, wt := range w.Tokens {
			merged = append(merged, viewToken(wt))
		}

		if n == 0 {
			break
		}
		i += n
	}
	out.Tokens = merged
	out.Calls = calls
	return out, nil
}

func callOnce(ctx context.Context, src chain.Source, req Request, tokens []common.Address) (wireResult, error) {
	s := req.Sources
	feeds := make([]wireFeedHint, len(s.Feeds))
	for i, f := range s.Feeds {
		feeds[i] = wireFeedHint{Token: f.Token, Aggregator: f.Aggregator, Quote: uint8(f.Quote)}
	}
	tiers := make([]*big.Int, len(s.FeeTiers))
	for i, t := range s.FeeTiers {
		tiers[i] = new(big.Int).SetUint64(uint64(t))
	}
	if tokens == nil {
		tokens = []common.Address{}
	}
	quotes := s.quoteAddresses()
	if quotes == nil {
		quotes = []common.Address{}
	}

	payload, err := encode(wireRequest{
		Tokens:         tokens,
		FeedRegistry:   s.FeedRegistry,
		NativeUsdFeed:  s.NativeUSDFeed,
		Feeds:          feeds,
		V3Factory:      s.V3Factory,
		FeeTiers:       tiers,
		V2Factory:      s.V2Factory,
		QuoteTokens:    quotes,
		TwapWindow:     req.TWAPWindow,
		GasPerCall:     new(big.Int).SetUint64(req.GasPerCall),
		MaxStringBytes: new(big.Int).SetUint64(req.MaxStringBytes),
	})
	if err != nil {
		return wireResult{}, err
	}
	if len(payload) > MaxPayloadBytes {
		return wireResult{}, fmt.Errorf("%w: %d bytes of initcode exceeds EIP-3860's %d",
			errTooBig, len(payload), MaxPayloadBytes)
	}
	ret, err := src.CallAtHead(ctx, ethereum.CallMsg{Data: payload})
	if err != nil {
		return wireResult{}, err
	}
	return decode(ret)
}

var errTooBig = errors.New("price: reply does not fit")

// tooBig matches what a node says when the reply cannot be handed back or the batch
// cost too much to run; same list internal/lens uses.
func tooBig(err error) bool {
	if errors.Is(err, errTooBig) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, needle := range []string{
		"max code size exceeded", "max initcode size exceeded", "code size",
		"out of gas", "gas required exceeds", "exceeds block gas limit",
		"intrinsic gas", "response size", "returned more than", "-32005",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// Wire -> public view
// --------------------------------------------------------------------------

func (w wireResult) view() *Snapshot {
	s := &Snapshot{
		ChainID:     u64(w.Chain.ChainId),
		BlockNumber: u64(w.Chain.BlockNumber),
		ParentHash:  w.Chain.ParentHash,
		Timestamp:   u64(w.Chain.Timestamp),
		Atomic:      true,
	}
	if w.Native.Aggregator != (common.Address{}) {
		f := viewFeed(w.Native)
		s.Native = &f
	}
	return s
}

func viewFeed(f wireFeedInfo) Feed {
	v := Feed{
		Aggregator:      f.Aggregator,
		Quote:           QuoteKind(f.Quote),
		ViaRegistry:     f.ViaRegistry,
		Answered:        f.Ok,
		StartedAt:       u64(f.StartedAt),
		UpdatedAt:       u64(f.UpdatedAt),
		RoundID:         f.RoundId,
		AnsweredInRound: f.AnsweredInRound,
		Description:     token.Clean(f.Description),
	}
	if f.Ok {
		v.Answer = f.Answer
	}
	if f.HasDecimals {
		d := f.Decimals
		v.Decimals = &d
	}
	return v
}

func viewPool(p wirePoolInfo) Pool {
	v := Pool{
		Address:          p.Pool,
		Kind:             PoolKind(p.Kind),
		Token0:           p.Token0,
		Token1:           p.Token1,
		Fee:              uint32(u64(p.Fee)),
		SqrtPriceX96:     p.SqrtPriceX96,
		Tick:             int32(i64(p.Tick)),
		Liquidity:        p.Liquidity,
		TWAPWindow:       p.TwapWindow,
		Reserve0:         p.Reserve0,
		Reserve1:         p.Reserve1,
		ReserveTimestamp: p.ReserveTimestamp,
	}
	if p.TwapWindow > 0 {
		v.TickCumulativeStart = p.TickCumulativeStart
		v.TickCumulativeEnd = p.TickCumulativeEnd
	}
	return v
}

func viewToken(t wireTokenPrices) TokenSources {
	v := TokenSources{
		Token:      t.Token,
		IsContract: t.IsContract,
		Symbol:     token.Clean(t.Symbol),
	}
	if t.HasDecimals {
		d := t.Decimals
		v.Decimals = &d
	}
	for _, f := range t.Feeds {
		v.Feeds = append(v.Feeds, viewFeed(f))
	}
	for _, p := range t.Pools {
		v.Pools = append(v.Pools, viewPool(p))
	}
	return v
}

func u64(v *big.Int) uint64 {
	if v == nil || !v.IsUint64() {
		return 0
	}
	return v.Uint64()
}

func i64(v *big.Int) int64 {
	if v == nil || !v.IsInt64() {
		return 0
	}
	return v.Int64()
}
