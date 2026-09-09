// Package lens reads an account's whole asset position in one call, using a contract
// that is never deployed.
//
// # Why a contract at all
//
// Answering "what does this address hold" from the RPC alone costs one eth_call per
// question: a balance, an allowance, a symbol, decimals, an owner, a URI. A wallet
// showing twenty tokens makes well over a hundred round trips, and the answers come
// from different block heights, so the "portfolio" it renders never existed as a
// state the chain was ever in.
//
// AssetLens does all of it inside a single EVM execution: one round trip, one block,
// one consistent snapshot, with every hostile token boxed into its own bounded
// staticcall.
//
// # Why deployless
//
// The lens is not deployed anywhere. eth_call with no `to` address executes creation
// code and returns whatever the constructor returns, so shipping the bytecode as the
// call payload turns a contract into a pure function:
//
//	eth_call({ data: creationCode || abi.encode(request) }, "latest")
//
// That keeps the project's premise intact. There is no deployment to fund on each
// chain, no address a user has to trust, no upgrade key, and nothing to coordinate
// with anybody: the lens works against any EVM node, on any chain, at the moment it
// compiles. It is the same posture as the rest of evm-scan — read public state from
// your own node, and require no permission from anyone to do it.
//
// # What that costs
//
// The trick inherits two consensus limits, because the constructor's return value is
// being treated as contract code:
//
//	EIP-170  reply   <= 24576 bytes   (exceeded => the whole call fails)
//	EIP-3860 payload <= 49152 bytes   (creation code is ~7.7KB of it)
//
// Query therefore batches tokens and halves the batch when a node rejects the reply,
// the same adaptive-window shape chain.SweepLogs uses for eth_getLogs.
package lens

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Skanislav/evm-scan/internal/chain"
	"github.com/Skanislav/evm-scan/internal/evmlog"
	"github.com/Skanislav/evm-scan/internal/token"
)

// Consensus ceilings the deployless trick runs into. They are the caller's problem,
// not the contract's, so they live here next to the batching that respects them.
const (
	// MaxReplyBytes is EIP-170's max code size: the constructor's return value is
	// treated as deployed code, so a larger reply fails the call outright.
	MaxReplyBytes = 24576
	// MaxPayloadBytes is EIP-3860's max initcode size, which bounds creation code
	// plus encoded arguments.
	MaxPayloadBytes = 49152
	// DefaultTokensPerCall is a conservative batch: ~30 tokens of metadata fit under
	// MaxReplyBytes, and halving handles the ones that do not.
	DefaultTokensPerCall = 24
)

// TokenQuery is one contract to read, plus the ids to look at inside it.
type TokenQuery struct {
	Token common.Address
	// IDs are ERC-721 ids to resolve or ERC-1155 ids to balance. Empty reads only
	// the contract-level facts.
	IDs []*big.Int
}

// Request is one account's worth of questions.
type Request struct {
	Account common.Address
	Tokens  []TokenQuery
	// Spenders are read as ERC-20 allowance(account, spender) and as ERC-721/1155
	// isApprovedForAll(account, spender).
	Spenders []common.Address
	// IncludeURI fetches tokenURI/uri for every id, which is large and usually not
	// what a balance view needs.
	IncludeURI bool
	// IncludeCode returns the account's bytecode. The 7702 delegation target comes
	// back either way.
	IncludeCode bool
	// EnumerateLimit walks up to this many of the account's ids on ERC-721Enumerable
	// contracts that were asked about without explicit ids.
	EnumerateLimit uint64
	// GasPerCall caps each staticcall the lens makes. Zero uses the contract's own
	// default.
	GasPerCall uint64
	// MaxStringBytes truncates every returned string. Zero uses the contract's
	// default; the contract clamps anything absurd.
	MaxStringBytes uint64
	// TokensPerCall bounds one eth_call. Zero uses DefaultTokensPerCall.
	TokensPerCall int
	// SkipNonce drops the eth_getTransactionCount that fills Account.Nonce, for
	// callers that only want token state.
	SkipNonce bool
}

// Account is the subject's own state at the queried block.
type Account struct {
	Address  common.Address
	Balance  *big.Int
	CodeHash common.Hash
	CodeSize uint64
	// IsContract is code that is not a delegation designator.
	IsContract bool
	// IsDelegated reports an EIP-7702 delegation, with Delegate naming the target.
	IsDelegated bool
	Delegate    common.Address
	Code        []byte
	// Nonce comes from eth_getTransactionCount, not from the lens: the EVM has no
	// opcode for another account's nonce. NonceKnown says whether that read landed.
	Nonce      uint64
	NonceKnown bool
}

// Allowance is what one spender may move on the account's behalf.
type Allowance struct {
	Spender common.Address
	// Amount is the ERC-20 allowance, or nil when the token did not answer — which
	// is not the same as an allowance of zero.
	Amount *big.Int
	// ApprovedForAll is the ERC-721/1155 operator approval.
	ApprovedForAll bool
}

// TokenID is one NFT id's state. A nil pointer means the contract did not answer.
type TokenID struct {
	ID       *big.Int
	Owner    *common.Address
	Balance  *big.Int
	Approved *common.Address
	URI      string
	// URITruncated reports that the string hit the size cap, so a consumer does not
	// treat a clipped URI as a real one.
	URITruncated bool
}

// Token is what one contract said about itself and about the account.
//
// Nil pointers are "the contract did not answer", never zero. A token that reverts
// on decimals() is not a token with zero decimals, and a wallet that renders the
// difference away is showing a number nobody vouched for.
type Token struct {
	Address        common.Address
	IsContract     bool
	Standard       evmlog.Standard
	SupportsERC165 bool
	IsERC721       bool
	IsERC1155      bool
	IsEnumerable   bool
	Symbol         string
	Name           string
	Decimals       *int16
	TotalSupply    *big.Int
	Balance        *big.Int
	Allowances     []Allowance
	IDs            []TokenID
}

// Metadata is the token package's view of this token, so a caller that already
// speaks that type does not need a second shape for the same three fields.
func (t Token) Metadata() token.Metadata {
	return token.Metadata{Symbol: t.Symbol, Name: t.Name, Decimals: t.Decimals}
}

// Result is one account's position as of one block.
type Result struct {
	ChainID     uint64
	BlockNumber uint64
	ParentHash  common.Hash
	Timestamp   uint64
	BaseFee     *big.Int
	Account     Account
	Tokens      []Token
	// Calls is how many eth_calls this took, including any a node rejected as too
	// large: the retries are part of what the answer cost.
	Calls int
	// Atomic reports that every token above was read at the same block. It goes
	// false when batching split the read and the head moved underneath it, which is
	// the one thing a single call would have guaranteed.
	Atomic bool
}

// Query reads the request against head state.
//
// Tokens are batched across as few eth_calls as the reply-size limit allows; a node
// that rejects a batch for size or gas gets a smaller one rather than an error.
func Query(ctx context.Context, src chain.Source, req Request) (*Result, error) {
	batch := req.TokensPerCall
	if batch <= 0 {
		batch = DefaultTokensPerCall
	}

	// The plan is what gets sent, the request is what was asked for. They diverge
	// when a batch has to be split, and every piece remembers which of the caller's
	// tokens it belongs to, so the reply can be reassembled in the caller's order.
	plans := make([]planned, len(req.Tokens))
	for i, t := range req.Tokens {
		plans[i] = planned{query: t, at: i}
	}
	merged := make([]*Token, len(req.Tokens))

	var (
		out   *Result
		first wireChainInfo
		calls int
	)
	for i := 0; i < len(plans) || i == 0; {
		n := min(batch, len(plans)-i)
		w, err := callOnce(ctx, src, req, queries(plans[i:i+n]))
		calls++
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if tooBig(err) {
				// Fewer tokens per call first; when it is already down to one, the
				// ids inside it are what does not fit.
				if n > 1 {
					batch = max(1, n/2)
					continue
				}
				if len(plans[i].query.IDs) > 1 {
					plans = splitIDs(plans, i)
					continue
				}
			}
			return nil, fmt.Errorf("lens: query %s (%d token(s) from index %d): %w", req.Account, n, i, err)
		}

		if out == nil {
			out = w.view()
			first = w.Chain
		} else if w.Chain.BlockNumber.Cmp(first.BlockNumber) != 0 || w.Chain.ParentHash != first.ParentHash {
			out.Atomic = false
		}
		for j, wt := range w.Tokens {
			at := plans[i+j].at
			tok := viewToken(wt, req.Spenders)
			if merged[at] == nil {
				merged[at] = &tok
				continue
			}
			// A token whose ids were split across calls: the contract-level facts
			// are the same either way, so only the ids accumulate.
			merged[at].IDs = append(merged[at].IDs, tok.IDs...)
		}

		if n == 0 {
			break
		}
		i += n
	}

	out.Tokens = make([]Token, 0, len(merged))
	for _, t := range merged {
		out.Tokens = append(out.Tokens, *t)
	}
	out.Calls = calls

	if !req.SkipNonce {
		if nonce, err := src.NonceAt(ctx, req.Account); err == nil {
			out.Account.Nonce, out.Account.NonceKnown = nonce, true
		}
	}
	return out, nil
}

// planned is one token query as it will actually be sent, and where its answer
// belongs in the caller's list.
type planned struct {
	query TokenQuery
	at    int
}

func queries(plans []planned) []TokenQuery {
	out := make([]TokenQuery, len(plans))
	for i, p := range plans {
		out[i] = p.query
	}
	return out
}

// splitIDs halves the ids of one planned query in place. Asking about a thousand NFT
// ids is an ordinary request, and it must not dead-end just because a single token's
// answer is what overflows the reply.
func splitIDs(plans []planned, i int) []planned {
	ids := plans[i].query.IDs
	half := len(ids) / 2

	left := plans[i]
	left.query.IDs = ids[:half]
	right := plans[i]
	right.query.IDs = ids[half:]

	out := make([]planned, 0, len(plans)+1)
	out = append(out, plans[:i]...)
	out = append(out, left, right)
	return append(out, plans[i+1:]...)
}

func callOnce(ctx context.Context, src chain.Source, req Request, tokens []TokenQuery) (wireResult, error) {
	payload, err := encode(wireRequest{
		Account:        req.Account,
		Spenders:       req.Spenders,
		Tokens:         wireTokens(tokens),
		IncludeUri:     req.IncludeURI,
		IncludeCode:    req.IncludeCode,
		EnumerateLimit: new(big.Int).SetUint64(req.EnumerateLimit),
		GasPerCall:     new(big.Int).SetUint64(req.GasPerCall),
		MaxStringBytes: new(big.Int).SetUint64(req.MaxStringBytes),
	})
	if err != nil {
		return wireResult{}, err
	}
	if len(payload) > MaxPayloadBytes {
		return wireResult{}, fmt.Errorf(
			"%w: %d bytes of initcode exceeds EIP-3860's %d; ask for fewer tokens or ids per call",
			errTooBig, len(payload), MaxPayloadBytes)
	}

	// No `to` address: the node runs the creation code and hands back what the
	// constructor returned.
	ret, err := src.CallAtHead(ctx, ethereum.CallMsg{Data: payload})
	if err != nil {
		return wireResult{}, err
	}
	return decode(ret)
}

func wireTokens(tokens []TokenQuery) []wireTokenQuery {
	out := make([]wireTokenQuery, len(tokens))
	for i, t := range tokens {
		ids := t.IDs
		if ids == nil {
			ids = []*big.Int{}
		}
		out[i] = wireTokenQuery{Token: t.Token, Ids: ids}
	}
	return out
}

var errTooBig = errors.New("lens: reply does not fit")

// tooBig matches the errors a node returns when the reply cannot be handed back or
// the batch cost too much to run. EIP-170 is the interesting one: the constructor's
// return value is code, so an oversized portfolio fails as "max code size exceeded"
// rather than as anything about the query.
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

func (w wireResult) view() *Result {
	res := &Result{
		ChainID:     u64(w.Chain.ChainId),
		BlockNumber: u64(w.Chain.BlockNumber),
		ParentHash:  w.Chain.ParentHash,
		Timestamp:   u64(w.Chain.Timestamp),
		BaseFee:     w.Chain.BaseFee,
		Atomic:      true,
		Account: Account{
			Address:     w.Account.Account,
			Balance:     w.Account.Balance,
			CodeHash:    w.Account.CodeHash,
			CodeSize:    u64(w.Account.CodeSize),
			IsContract:  w.Account.IsContract,
			IsDelegated: w.Account.IsDelegated,
			Delegate:    w.Account.Delegate,
			Code:        w.Account.Code,
		},
	}
	return res
}

func viewToken(t wireTokenInfo, spenders []common.Address) Token {
	v := Token{
		Address:        t.Token,
		IsContract:     t.IsContract,
		Standard:       evmlog.Standard(t.Standard),
		SupportsERC165: t.SupportsErc165,
		IsERC721:       t.IsErc721,
		IsERC1155:      t.IsErc1155,
		IsEnumerable:   t.IsEnumerable,
		// A token's own strings are attacker-controlled: clean them once, here, so
		// nothing downstream has to remember to.
		Symbol: token.Clean(t.Symbol),
		Name:   token.Clean(t.Name),
	}
	if t.HasDecimals {
		d := int16(t.Decimals)
		v.Decimals = &d
	}
	if t.HasTotalSupply {
		v.TotalSupply = t.TotalSupply
	}
	if t.HasBalance {
		v.Balance = t.Balance
	}

	for j, spender := range spenders {
		a := Allowance{Spender: spender}
		if j < len(t.AllowanceKnown) && t.AllowanceKnown[j] {
			a.Amount = t.Allowances[j]
		}
		if j < len(t.ApprovedForAll) {
			a.ApprovedForAll = t.ApprovedForAll[j]
		}
		v.Allowances = append(v.Allowances, a)
	}

	v.IDs = make([]TokenID, len(t.Ids))
	for j, id := range t.Ids {
		e := TokenID{ID: id.Id, URI: token.Clean(id.Uri), URITruncated: id.UriTruncated}
		if id.OwnerKnown {
			owner := id.Owner
			e.Owner = &owner
		}
		if id.BalanceKnown {
			e.Balance = id.Balance
		}
		if id.ApprovedKnown {
			approved := id.Approved
			e.Approved = &approved
		}
		v.IDs[j] = e
	}
	return v
}

func u64(v *big.Int) uint64 {
	if v == nil || !v.IsUint64() {
		return 0
	}
	return v.Uint64()
}
